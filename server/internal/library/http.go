package library

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const metadataLimit = 16 << 10

type Server struct {
	DB      *gorm.DB
	DataDir string
	BaseURL string
	// 素材存储。nil 表示没配 OSS——此时素材落在本地磁盘（本地开发/测试/尚未配置的部署）。
	OSS         AssetStore
	uploadSlots chan struct{}
}

func NewServer(db *gorm.DB, dir, base string) (*Server, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("PUBLIC_BASE_URL must be an HTTPS base URL")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	oss, err := OSSFromEnv()
	if err != nil {
		return nil, err
	}
	// 注意别把"值为 nil 的 *OSSClient"塞进接口——那样接口非 nil，s.OSS != nil 会成立，
	// 于是没配 OSS 的环境会走进 OSS 分支然后空指针。
	var store AssetStore
	if oss != nil {
		store = oss
	}
	return &Server{DB: db, DataDir: dir, BaseURL: strings.TrimRight(base, "/"), OSS: store, uploadSlots: make(chan struct{}, 4)}, nil
}

type identityKey struct{}

func userID(r *http.Request) uint64 { v, _ := r.Context().Value(identityKey{}).(uint64); return v }

// badCredentialKey 标记"带了 Authorization 但那个凭据用不了"。
//
// 这种请求按匿名继续处理，而不是当场 401：客户端环境里留着一个过期 token 是很常见的，
// 一旦无效凭据能让**公开**端点也 401，客户端连 /healthz 和公开清单都读不到——恰恰是
// 想排查"我的 token 为什么被拒"的人最需要的那两个接口（实测踩过）。
// 真正需要凭据的接口仍会拒绝，只是能区分"没带"和"带了但无效"。
type badCredentialKey struct{}

func badCredential(r *http.Request) bool {
	v, _ := r.Context().Value(badCredentialKey{}).(bool)
	return v
}

// requireCredential 是"本接口必须有有效凭据"的统一出口。
func requireCredential(w http.ResponseWriter, r *http.Request) bool {
	if userID(r) != 0 {
		return true
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	if badCredential(r) {
		fail(w, 401, "invalid token")
	} else {
		fail(w, 401, "token required")
	}
	return false
}

// bearerValue returns the raw credential without validating it; callers must
// only use it after the wrapper has authenticated the request.
func bearerValue(r *http.Request) string {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(value, "Bearer ")
}
func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /gamesource/v1/health", s.health)
	mux.HandleFunc("GET /gamesource/v1/index.json", s.index)
	mux.HandleFunc("GET /api/v1/games", s.list)
	mux.HandleFunc("GET /api/v1/games/{id}", s.get)
	mux.HandleFunc("GET /api/v1/games/{id}/package.pck", s.download)
	mux.HandleFunc("GET /api/v1/games/{id}/package.game", s.download)
	mux.HandleFunc("GET /api/v1/games/{id}/assets/{name}", s.asset)
	mux.HandleFunc("PUT /api/v1/games/{id}", s.put)
	mux.HandleFunc("PATCH /api/v1/games/{id}", s.patch)
	mux.HandleFunc("DELETE /api/v1/games/{id}", s.delete)
	mux.HandleFunc("POST /api/v1/games/{id}/play-sessions", s.createPlaySession)
	mux.HandleFunc("POST /api/v1/games/{id}/scores", s.submitScore)
	mux.HandleFunc("GET /api/v1/games/{id}/leaderboard", s.leaderboard)
	mux.HandleFunc("GET /sdk/v1/game-stop.js", s.sdkScript)
	mux.HandleFunc("GET /leaderboards/{id}", s.leaderboardPage)
	mux.HandleFunc("GET /connect", s.connectPage)
	mux.HandleFunc("GET /connect.js", s.connectScript)
	mux.HandleFunc("POST /api/v1/auth/register", s.registerAccount)
	mux.HandleFunc("POST /api/v1/auth/login", s.loginAccount)
	mux.HandleFunc("POST /api/v1/auth/logout", s.logoutAccount)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("DELETE /api/v1/me", s.closeMyAccount)
	mux.HandleFunc("POST /api/v1/me/tokens", s.createMyToken)
	mux.HandleFunc("GET /api/v1/me/tokens", s.listMyTokens)
	mux.HandleFunc("DELETE /api/v1/me/tokens/{id}", s.revokeMyToken)
	mux.HandleFunc("GET /register", s.registerPage)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /account", s.accountPage)
	mux.HandleFunc("GET /auth.js", s.authScript)
	mux.HandleFunc("POST /api/v1/games/{id}/device-sessions", s.createDeviceSession)
	mux.HandleFunc("POST /api/v1/games/{id}/device-sessions/poll", s.pollDeviceSession)
	mux.HandleFunc("POST /api/v1/device-sessions/authorize", s.authorizeDeviceSession)
	mux.HandleFunc("GET /device-connect", s.deviceConnectPage)
	mux.HandleFunc("GET /device-connect.js", s.deviceConnectScript)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Vary", "Authorization, X-Play-Token")
		if platformCORS(w, r) {
			return
		}
		// A credential that is present but unusable marks the request and lets it
		// continue as anonymous - see badCredentialKey. Missing identity is then
		// rejected by the handlers that actually need it.
		if a, ok := r.Header["Authorization"]; ok {
			usable := len(a) == 1 && strings.HasPrefix(a[0], "Bearer ") && len(a[0]) == 53
			var raw string
			if usable {
				raw = strings.TrimPrefix(a[0], "Bearer ")
			}
			now := time.Now().UTC()
			// Two credential kinds share the Bearer slot but never share powers:
			// an API token can act for the account, a login session can only
			// manage that account's own tokens. Neither is the other.
			switch {
			case usable && apiTokenPattern.MatchString(raw):
				var t Token
				err := s.DB.WithContext(r.Context()).Where("hash = ? AND revoked_at IS NULL AND expires_at > ?", tokenHash(raw), now).First(&t).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					fail(w, 503, "authentication unavailable")
					return
				}
				if errors.Is(err, gorm.ErrRecordNotFound) {
					r = r.WithContext(context.WithValue(r.Context(), badCredentialKey{}, true))
					break
				}
				r = r.WithContext(context.WithValue(r.Context(), identityKey{}, t.UserID))
				r = r.WithContext(context.WithValue(r.Context(), sourceTokenKey{}, t.ID))
			case usable && sessionPattern.MatchString(raw):
				var session LoginSession
				err := s.DB.WithContext(r.Context()).Where("hash = ? AND expires_at > ?", tokenHash(raw), now).First(&session).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					fail(w, 503, "authentication unavailable")
					return
				}
				if errors.Is(err, gorm.ErrRecordNotFound) {
					r = r.WithContext(context.WithValue(r.Context(), badCredentialKey{}, true))
					break
				}
				r = r.WithContext(context.WithValue(r.Context(), identityKey{}, session.UserID))
				r = r.WithContext(context.WithValue(r.Context(), loginSessionKey{}, session.UserID))
			default:
				r = r.WithContext(context.WithValue(r.Context(), badCredentialKey{}, true))
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	db, err := s.DB.DB()
	if err != nil || db.PingContext(ctx) != nil {
		fail(w, 503, "not ready")
		return
	}
	jsonResponse(w, 200, map[string]bool{"ok": true})
}
func (s *Server) representation(g Game) map[string]any {
	var m Metadata
	_ = json.Unmarshal([]byte(g.Metadata), &m)
	if m.Tags == nil {
		m.Tags = []string{}
	}
	format := g.Format
	if format == "" {
		format = "pck"
	}
	// 下载路径按格式区分：容器与裸包不能共用一个 URL，否则客户端无法只看地址判断拿了什么。
	suffix := "package.pck"
	if format != "pck" {
		suffix = "package.game"
	}
	// 包在 OSS 上就签一个临时地址（桶是私有的，没有别的读法）；老记录仍走平台自己的端点。
	packageURL := s.BaseURL + "/api/v1/games/" + g.ID + "/" + suffix
	if g.PackageKey != "" && s.OSS != nil {
		packageURL = s.OSS.SignedGetURLNow(g.PackageKey, "")
	}
	pack := map[string]any{
		"url":  packageURL,
		"size": g.Size, "sha256": g.SHA256, "format": format,
	}
	out := map[string]any{"id": g.ID, "ownerId": g.OwnerID, "title": m.Title, "subtitle": m.Subtitle, "description": m.Description, "tags": m.Tags, "accent": m.Accent, "engine": m.Engine, "visibility": g.Visibility, "status": "ready", "createdAt": g.CreatedAt, "updatedAt": g.UpdatedAt, "package": pack}
	// 上传回执：告诉发布方"服务端收到的字节是什么"。容器提交时它与 package.* 不同
	// （收到的是容器、分发的是解出来的 pck），所以必须分开报，否则发布方无法判断
	// "服务端到底有没有收对"，只能瞎猜。
	if g.ReceivedSHA256 != "" {
		out["received"] = map[string]any{"size": g.ReceivedSize, "sha256": g.ReceivedSHA256}
	}
	// 素材与出处：游戏源协议里 cover/coverSha256 本来就有位置，只是以前一直是空的。
	var assets GameAssets
	if json.Unmarshal([]byte(g.Assets), &assets) == nil {
		if assets.Cover != nil {
			out["cover"] = s.assetURL(g.ID, *assets.Cover, CoverProcess)
			out["coverSha256"] = assets.Cover.SHA256
		}
		shots := make([]string, 0, len(assets.Screenshots))
		for _, shot := range assets.Screenshots {
			shots = append(shots, s.assetURL(g.ID, shot, ShotProcess))
		}
		if len(shots) > 0 {
			out["screenshots"] = shots
		}
	}
	var src BundleSource
	if json.Unmarshal([]byte(g.Source), &src) == nil && src.Repo != "" {
		out["source"] = src
	}
	return out
}
func (s *Server) games(r *http.Request, publicOnly bool) ([]map[string]any, error) {
	db := s.DB.WithContext(r.Context()).Where("deleted = ?", false)
	if publicOnly || userID(r) == 0 {
		db = db.Where("visibility = ?", "public")
	} else {
		db = db.Where("visibility = ? OR owner_id = ?", "public", userID(r))
	}
	var games []Game
	if err := db.Order("id ASC").Find(&games).Error; err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(games))
	for _, g := range games {
		out = append(out, s.representation(g))
	}
	return out, nil
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	g, err := s.games(r, false)
	if err != nil {
		fail(w, 503, "database unavailable")
		return
	}
	jsonResponse(w, 200, map[string]any{"games": g})
}
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	g, err := s.games(r, true)
	if err != nil {
		fail(w, 503, "database unavailable")
		return
	}
	modified := time.Unix(0, 0).UTC()
	for _, game := range g {
		if updated, ok := game["updatedAt"].(time.Time); ok && updated.After(modified) {
			modified = updated
		}
	}
	body, err := json.Marshal(map[string]any{"protocol": "gamesource/1", "sourceName": "Game Stop", "ttlSeconds": 0, "generatedAt": modified, "games": g})
	if err != nil {
		fail(w, 503, "catalog unavailable")
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
	// Only the full representation hash is a validator: second-granularity dates
	// cannot safely distinguish a visibility change or deletion in the same second.
	for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
func (s *Server) readable(w http.ResponseWriter, r *http.Request) (Game, bool) {
	var g Game
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		fail(w, 400, "invalid game id")
		return g, false
	}
	err := s.DB.WithContext(r.Context()).Where("id = ? AND deleted = ?", id, false).First(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && g.Visibility != "public" && g.OwnerID != userID(r)) {
		fail(w, 404, "game not found")
		return g, false
	}
	if err != nil {
		fail(w, 503, "database unavailable")
		return g, false
	}
	return g, true
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if g, ok := s.readable(w, r); ok {
		jsonResponse(w, 200, s.representation(g))
	}
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	g, ok := s.readable(w, r)
	if !ok {
		return
	}
	// 新记录只在 OSS：签名地址才是真正的读法。这里 302 过去，让浏览器/curl -L 仍然能用；
	// CLI 走的是清单里的 package.url，不依赖这个跳转。
	if g.PackageKey != "" {
		if s.OSS == nil {
			fail(w, 503, "package unavailable")
			return
		}
		http.Redirect(w, r, s.OSS.SignedGetURLNow(g.PackageKey, ""), http.StatusFound)
		return
	}
	f, err := os.Open(filepath.Join(s.DataDir, g.Filename))
	if err != nil {
		fail(w, 503, "package unavailable")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+g.ID+`.pck"`)
	w.Header().Set("ETag", `"`+g.SHA256+`"`)
	w.Header().Set("X-Checksum-SHA256", g.SHA256)
	http.ServeContent(w, r, g.ID+".pck", g.UpdatedAt, f)
}
func writable(w http.ResponseWriter, r *http.Request) bool {
	if !requireCredential(w, r) {
		return false
	}
	if !idPattern.MatchString(r.PathValue("id")) {
		fail(w, 400, "invalid game id")
		return false
	}
	return true
}
func decodeJSON(b []byte, v any) error {
	if !json.Valid(b) {
		return errors.New("invalid JSON")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// metadataHeader 解出 `X-Game-Metadata` 里的 Base64 JSON；空或缺席都算不合法。
func metadataHeader(r *http.Request) ([]byte, bool) {
	headers := r.Header.Values("X-Game-Metadata")
	if len(headers) != 1 || headers[0] == "" || len(headers[0]) > base64.StdEncoding.EncodedLen(metadataLimit) {
		return nil, false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(headers[0])
	if err != nil {
		return nil, false
	}
	return b, true
}

var errOwner = errors.New("owner conflict")
var errMissing = errors.New("missing")

func mutationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOwner), errors.Is(err, gorm.ErrDuplicatedKey):
		fail(w, 409, "game id belongs to another user or conflicting write")
	case errors.Is(err, errMissing):
		fail(w, 404, "game not found")
	default:
		fail(w, 503, "write unavailable")
	}
}
func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	if !writable(w, r) {
		return
	}
	// 先看魔数再决定按哪种格式收：迁移期容器与裸 pck 并存（ADR-0011）。
	buffered := bufio.NewReader(http.MaxBytesReader(w, r.Body, MaxPackageBytes))
	head, err := buffered.Peek(4)
	if err != nil {
		fail(w, 400, "upload incomplete")
		return
	}
	isBundle := LooksLikeBundle(head)
	var m Metadata
	if isBundle {
		// 容器的元数据在包里；再给一份**有内容**的头就是两个真源，直接拒收。
		// 空头不算（它没有携带任何信息，早先的 CLI 就会带上一个空的）。
		if _, ok := metadataHeader(r); ok {
			fail(w, 400, "metadata must live inside the bundle")
			return
		}
	} else {
		raw, ok := metadataHeader(r)
		if !ok || decodeJSON(raw, &m) != nil || m.Validate() != nil {
			fail(w, 400, "invalid metadata")
			return
		}
	}
	if r.ContentLength > MaxPackageBytes {
		fail(w, 413, "package exceeds 512 MiB")
		return
	}
	// Reject known collisions before consuming a potentially large body. The transaction
	// repeats this check under a row lock, and the primary key protects absent-row races.
	var old Game
	err = s.DB.WithContext(r.Context()).Where("id = ?", r.PathValue("id")).First(&old).Error
	if err == nil && old.OwnerID != userID(r) {
		mutationError(w, errOwner)
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		mutationError(w, err)
		return
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		fail(w, 503, "upload capacity reached")
		return
	}
	f, err := os.CreateTemp(s.DataDir, "upload-*.part")
	if err != nil {
		log.Printf("upload %s: 无法在数据目录建临时文件: %v", r.PathValue("id"), err)
		fail(w, 503, "storage unavailable")
		return
	}
	temp := f.Name()
	defer os.Remove(temp)
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), buffered)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			fail(w, 413, "package exceeds 512 MiB")
		} else {
			fail(w, 400, "upload incomplete")
		}
		return
	}
	if r.ContentLength >= 0 && n != r.ContentLength {
		fail(w, 400, "upload incomplete")
		return
	}
	// 容器走容器校验（含包内 pck 校验），裸包沿用原来的 pck 校验。
	format := "pck"
	var source BundleSource
	var assets GameAssets
	cleanupAssets := func() {}
	// 容器解出来的东西要活到这个分支之外——后面上传 pck 还要用。
	var ingested ingestedBundle
	if isBundle {
		var cleanup func()
		ingested, cleanup, err = s.ingestBundle(f, n, r.PathValue("id"))
		if err != nil {
			fail(w, 400, "invalid or unsafe game bundle")
			return
		}
		cleanupAssets = cleanup
		if ingested.EntryPath != "" {
			entryPath := ingested.EntryPath
			defer os.Remove(entryPath)
		}
		m = ingested.Metadata
		source = ingested.Source
		assets = ingested.Assets
		format = "game/1"
	} else {
		version, err := InspectPCK(f, n)
		if err != nil {
			fail(w, 400, "invalid or unsafe PCK")
			return
		}
		if version != m.Engine.Version {
			fail(w, 400, "engine version does not match PCK")
			return
		}
	}
	// 包体要先于 Close 决定去向：OSS 上传得从头读这个临时文件（关掉之后再 Seek 会失败）。
	//
	// 分发对象是什么，取决于有没有配 OSS：
	//   配了 OSS + 容器提交 → 分发**解出来的 pck**（`.game` 只是上传方与平台之间的协议，
	//                        平台拆成"图片 + pck"再对外发），因此 sha/大小都得按 pck 算；
	//   其余情况           → 分发刚收上来的那个文件本身（裸 pck，或没配 OSS 时的容器）。
	containerDigest := hex.EncodeToString(hash.Sum(nil))
	distDigest, distSize, distExt := containerDigest, n, ".pck"
	if format != "pck" {
		distExt = ".game"
	}
	packageKey := ""
	if s.OSS != nil {
		if isBundle {
			distDigest, distSize, distExt = ingested.EntrySHA256, ingested.EntrySize, ".pck"
		}
		packageKey = s.OSS.PackageKey(r.PathValue("id"), distDigest, distExt)
		if isBundle {
			pck, e := os.Open(ingested.EntryPath)
			if e != nil {
				log.Printf("upload %s: 打不开解出来的 pck %q: %v", r.PathValue("id"), ingested.EntryPath, e)
				fail(w, 503, "storage unavailable")
				return
			}
			defer pck.Close()
			err = s.OSS.PutObjectStream(packageKey, pck, distSize, "application/octet-stream")
		} else {
			if _, err = f.Seek(0, io.SeekStart); err == nil {
				err = s.OSS.PutObjectStream(packageKey, f, distSize, "application/octet-stream")
			}
		}
		if err != nil {
			log.Printf("upload %s: 写对象存储失败 key=%s: %v", r.PathValue("id"), packageKey, err)
			fail(w, 503, "object storage unavailable")
			return
		}
		// 对外分发的是 pck，所以清单里的 format 也是 pck——客户端直接下这个，不用解包。
		format = "pck"
	}
	if err = f.Sync(); err != nil {
		log.Printf("upload %s: Sync 失败: %v", r.PathValue("id"), err)
		fail(w, 503, "storage unavailable")
		return
	}
	if err = f.Close(); err != nil {
		log.Printf("upload %s: Close 失败: %v", r.PathValue("id"), err)
		fail(w, 503, "storage unavailable")
		return
	}
	name, err := randomHex(24)
	if err != nil {
		fail(w, 503, "storage unavailable")
		return
	}
	ext := ".pck"
	if format == "pck" {
		name += ext
	} else {
		ext = ".game"
		name += ext
	}
	_ = ext
	// 本地形态才需要改名落盘；OSS 形态上面已经传完，不再留本地副本
	// （留一份等于存两份，"挪出主机"只做了一半）。
	dest := ""
	if packageKey == "" {
		dest = filepath.Join(s.DataDir, name)
		if err = os.Rename(temp, dest); err != nil {
			fail(w, 503, "storage unavailable")
			return
		}
	}
	published := false
	defer func() {
		if !published {
			if dest != "" {
				_ = os.Remove(dest)
			}
			if packageKey != "" {
				_ = s.OSS.DeleteObject(packageKey)
			}
			cleanupAssets()
		}
	}()
	var result Game
	created := false
	err = s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var g Game
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", r.PathValue("id")).First(&g).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			created = true
			g = Game{ID: r.PathValue("id"), OwnerID: userID(r), Visibility: "private"}
		} else if e != nil {
			return e
		} else if g.OwnerID != userID(r) {
			return errOwner
		}
		if m.Visibility != "" {
			g.Visibility = m.Visibility
		}
		m.Visibility = g.Visibility
		mb, _ := json.Marshal(m)
		g.Metadata = string(mb)
		if packageKey != "" {
			// 只在 OSS：本地文件名留空，读的时候一眼能看出该去哪儿取。
			g.Filename = ""
			g.PackageKey = packageKey
		} else {
			g.Filename = name
			g.PackageKey = ""
		}
		// 记录的 sha/大小描述的是**分发对象**（配了 OSS 就是 pck），
		// 客户端拿它校验下载到的东西，口径必须与 package.url 指的东西一致。
		g.Size = distSize
		g.SHA256 = distDigest
		g.ReceivedSize = n
		g.ReceivedSHA256 = containerDigest
		g.Format = format
		assetsJSON, _ := json.Marshal(assets)
		if isBundle {
			g.Assets = string(assetsJSON)
		} else {
			// 裸 pck 上传本来就没有素材，不能把上一个容器的封面留在记录里。
			// 判据必须是"这次是不是容器提交"，**不能**看 format：容器提交后对外分发的是
			// pck（format=pck），照 format 判会把刚存好的素材又抹掉（真踩过）。
			g.Assets = ""
		}
		sourceJSON, _ := json.Marshal(source)
		g.Source = string(sourceJSON)
		g.Deleted = false
		if created {
			e = tx.Create(&g).Error
		} else {
			// 列清单是**写死的**：新增字段忘了加进来，重传（走 UPDATE）时就会静默不落库。
			// 这个坑真踩过——`package_key` 没进清单，于是包明明传上了 OSS，记录里却是空的，
			// 对外还给出平台端点地址 + 荒谬的 Content-Length。
			e = tx.Model(&Game{}).Where("id = ? AND owner_id = ?", g.ID, userID(r)).Select(
				"metadata", "filename", "package_key", "size", "sha256",
				"received_size", "received_sha256", "format", "assets", "source",
				"visibility", "deleted", "updated_at").Updates(&g).Error
		}
		result = g
		return e
	})
	if err != nil {
		mutationError(w, err)
		return
	}
	published = true
	// Historical immutable files deliberately remain: a request that read the old
	// record can still open it after replacement. Offline GC requires a grace period.
	status := 200
	if created {
		status = 201
	}
	jsonResponse(w, status, s.representation(result))
}
func (s *Server) patch(w http.ResponseWriter, r *http.Request) {
	if !writable(w, r) {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, metadataLimit))
	if err != nil {
		fail(w, 413, "metadata too large")
		return
	}
	var changes map[string]json.RawMessage
	if decodeJSON(b, &changes) != nil || len(changes) == 0 {
		fail(w, 400, "invalid metadata")
		return
	}
	for k, v := range changes {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			fail(w, 400, "null metadata not allowed")
			return
		}
		switch k {
		case "title", "subtitle", "description", "tags", "accent", "visibility":
		default:
			fail(w, 400, "field cannot be changed")
			return
		}
	}
	var result Game
	invalid := false
	err = s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var g Game
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted = ?", r.PathValue("id"), false).First(&g).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return errMissing
		}
		if e != nil {
			return e
		}
		if g.OwnerID != userID(r) {
			return errOwner
		}
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(g.Metadata), &fields)
		for k, v := range changes {
			fields[k] = v
		}
		merged, _ := json.Marshal(fields)
		var m Metadata
		if decodeJSON(merged, &m) != nil || m.Validate() != nil {
			invalid = true
			return errors.New("invalid metadata")
		}
		if m.Visibility != "" {
			g.Visibility = m.Visibility
		}
		g.Metadata = string(merged)
		e = tx.Model(&Game{}).Where("id = ? AND owner_id = ?", g.ID, userID(r)).Select("metadata", "visibility", "updated_at").Updates(&g).Error
		result = g
		return e
	})
	if invalid {
		fail(w, 400, "invalid metadata")
		return
	}
	if err != nil {
		mutationError(w, err)
		return
	}
	jsonResponse(w, 200, s.representation(result))
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if !writable(w, r) {
		return
	}
	err := s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var g Game
		e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted = ?", r.PathValue("id"), false).First(&g).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return errMissing
		}
		if e != nil {
			return e
		}
		if g.OwnerID != userID(r) {
			return errOwner
		}
		return tx.Model(&Game{}).Where("id = ? AND owner_id = ?", g.ID, userID(r)).Updates(map[string]any{"deleted": true, "visibility": "private"}).Error
	})
	if err != nil {
		mutationError(w, err)
		return
	}
	w.WriteHeader(204)
}
