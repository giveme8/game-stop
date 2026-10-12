package library

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gorm.io/gorm"
)

type uploadFixture struct {
	db    *gorm.DB
	h     http.Handler
	token string
	s     *Server
}

// 上传一个 `.game`（或裸 pck）并返回响应。
func (f *uploadFixture) put(t *testing.T, id string, body []byte, metadata string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/api/v1/games/"+id, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/octet-stream")
	if metadata != "" {
		r.Header.Set("X-Game-Metadata", metadata)
	}
	r.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *uploadFixture) get(path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	return newUploadFixtureWith(t, testDB(t))
}

func newUploadFixtureWith(t *testing.T, db *gorm.DB) *uploadFixture {
	t.Helper()
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	u, err := CreateUser(db, "bundle-publisher")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := CreateToken(db, u.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &uploadFixture{db: db, h: s.Handler(), token: token, s: s}
}

// exerciseBundleUpload 是容器上传的**主干路径**，SQLite 与 MySQL 都要跑。
//
// 为什么要两边都跑：`games.assets/source` 这类 TEXT 列在 MySQL 上有额外限制
// （不允许默认值），SQLite 根本不会报错——CI 已经抓到过一次这种"只差一个数据库"的失败。
func exerciseBundleUpload(t *testing.T, db *gorm.DB) {
	t.Helper()
	f := newUploadFixtureWith(t, db)
	gameID := "bundle-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	raw := validBundle(t, func(m map[string]any) {
		m["id"] = gameID
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	}, zipEntry{name: "assets/shot-1.png", data: []byte(testPNG)})

	w := f.put(t, gameID, raw, "")
	if w.Code != 201 {
		t.Fatalf("bundle upload on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	pack, _ := body["package"].(map[string]any)
	if pack["format"] != "game/1" || body["cover"] == nil || body["source"] == nil {
		t.Fatalf("bundle projection wrong on %s: %v", db.Dialector.Name(), body)
	}
	if dl := f.get("/api/v1/games/"+gameID+"/package.game", f.token); dl.Code != 200 {
		t.Fatalf("bundle download on %s: %d", db.Dialector.Name(), dl.Code)
	}
	// 容器不合法时必须整单拒绝、不留记录。
	badID := gameID + "-bad"
	bad := validBundle(t, func(m map[string]any) {
		m["id"] = badID
	}, zipEntry{name: "content/evil.exe", data: []byte("MZ")})
	if w := f.put(t, badID, bad, ""); w.Code != 400 {
		t.Fatalf("unsafe bundle accepted on %s: %d", db.Dialector.Name(), w.Code)
	}
	var count int64
	if err := db.Model(&Game{}).Where("id = ?", badID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected bundle left a record on %s: %d %v", db.Dialector.Name(), count, err)
	}
}

func TestBundleUploadSQLite(t *testing.T) { exerciseBundleUpload(t, testDB(t)) }

func TestUploadBundleStoresAssetsAndMetadata(t *testing.T) {
	f := newUploadFixture(t)
	raw := validBundle(t, func(m map[string]any) {
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	}, zipEntry{name: "assets/shot-1.png", data: []byte(testPNG)})

	w := f.put(t, "demo-game", raw, "")
	if w.Code != 201 {
		t.Fatalf("upload bundle: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	pack, _ := body["package"].(map[string]any)
	if pack["format"] != "game/1" {
		t.Fatalf("format not reported: %v", pack)
	}
	if pack["url"] != "https://games.example/api/v1/games/demo-game/package.game" {
		t.Fatalf("wrong package url: %v", pack["url"])
	}
	// 元数据来自包内的 game.json，而不是任何请求头。
	if body["title"] != "演示游戏" {
		t.Fatalf("metadata not taken from the bundle: %v", body["title"])
	}
	cover, _ := body["cover"].(string)
	if cover == "" || body["coverSha256"] == "" {
		t.Fatalf("cover missing: %v", body)
	}
	shots, _ := body["screenshots"].([]any)
	if len(shots) != 1 {
		t.Fatalf("screenshots missing: %v", body["screenshots"])
	}
	source, _ := body["source"].(map[string]any)
	if source["repo"] != "https://github.com/example/demo" {
		t.Fatalf("source missing: %v", body["source"])
	}

	// 下载：容器本身按 package.game 提供，字节与上传一致（游戏默认私有，用所有者身份）。
	dl := f.get("/api/v1/games/demo-game/package.game", f.token)
	if dl.Code != 200 || !bytes.Equal(dl.Body.Bytes(), raw) {
		t.Fatalf("bundle download: %d %d bytes", dl.Code, dl.Body.Len())
	}
	if dl.Header().Get("X-Checksum-SHA256") != pack["sha256"] {
		t.Fatal("checksum header mismatch")
	}
	// 封面单独可取，且 Content-Type 来自魔数。
	img := f.get(cover[len("https://games.example"):], f.token)
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("cover: %d %s", img.Code, img.Header().Get("Content-Type"))
	}
}

func TestUploadBundleRejectsMetadataHeader(t *testing.T) {
	f := newUploadFixture(t)
	meta := base64.StdEncoding.EncodeToString([]byte(`{"title":"x","engine":{"name":"godot","version":"4.7"}}`))
	w := f.put(t, "demo-game", validBundle(t, nil), meta)
	if w.Code != 400 {
		t.Fatalf("bundle with a metadata header must be refused: %d %s", w.Code, w.Body.String())
	}
	// 空头没有携带任何信息，不能当成"第二个真源"：早先的 CLI 就会带一个空的，
	// 结果每个容器上传都撞 400。
	empty := f.put(t, "demo-game", validBundle(t, nil), " ")
	if empty.Code == 400 {
		t.Fatalf("an empty metadata header must not be treated as a second source: %s", empty.Body.String())
	}
}

func TestUploadBundleRejectsEngineMismatch(t *testing.T) {
	f := newUploadFixture(t)
	// 包内 pck 是 4.7，但 game.json 声称 4.6。
	raw := validBundle(t, func(m map[string]any) {
		m["engine"] = map[string]any{"name": "godot", "version": "4.6"}
	})
	w := f.put(t, "demo-game", raw, "")
	if w.Code != 400 {
		t.Fatalf("engine mismatch accepted: %d %s", w.Code, w.Body.String())
	}
	// 失败不得留下记录，也不得留下素材。
	var count int64
	if err := f.db.Model(&Game{}).Where("id = ?", "demo-game").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed upload left a record: %d %v", count, err)
	}
	var assets int64
	if err := f.db.Model(&Game{}).Where("assets <> ''").Count(&assets).Error; err != nil || assets != 0 {
		t.Fatalf("failed upload left assets: %d", assets)
	}
}

func TestUploadBundleRejectsUnsafeContainerWithoutRecord(t *testing.T) {
	f := newUploadFixture(t)
	raw := validBundle(t, nil, zipEntry{name: "content/evil.exe", data: []byte("MZ")})
	w := f.put(t, "demo-game", raw, "")
	if w.Code != 400 {
		t.Fatalf("unsafe bundle accepted: %d", w.Code)
	}
	var count int64
	if err := f.db.Model(&Game{}).Where("id = ?", "demo-game").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unsafe upload left a record: %d", count)
	}
}

func TestLegacyPCKUploadStillWorks(t *testing.T) {
	f := newUploadFixture(t)
	meta := base64.StdEncoding.EncodeToString([]byte(`{"title":"裸包游戏","engine":{"name":"godot","version":"4.7"}}`))
	w := f.put(t, "legacy-game", validEntry(), meta)
	if w.Code != 201 {
		t.Fatalf("legacy pck upload: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	pack, _ := body["package"].(map[string]any)
	if pack["format"] != "pck" {
		t.Fatalf("legacy format not reported: %v", pack)
	}
	if _, hasCover := body["cover"]; hasCover {
		t.Fatal("legacy pck must not claim a cover")
	}
	if w := f.get("/api/v1/games/legacy-game/package.pck", f.token); w.Code != 200 {
		t.Fatalf("legacy download: %d", w.Code)
	}
}

func TestBundleAssetsRespectVisibility(t *testing.T) {
	f := newUploadFixture(t)
	w := f.put(t, "demo-game", validBundle(t, nil), "")
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	cover, _ := body["cover"].(string)
	path := cover[len("https://games.example"):]

	// 默认私有：匿名连封面都拿不到（404，而不是 403——不确认这个游戏存在）。
	if anon := f.get(path, ""); anon.Code != 404 {
		t.Fatalf("anonymous read a private cover: %d", anon.Code)
	}
	if owner := f.get(path, f.token); owner.Code != 200 {
		t.Fatalf("owner could not read its own cover: %d", owner.Code)
	}
	// 切成公开后匿名可读。
	if patch := f.patchVisibility(t, "demo-game"); patch != 200 {
		t.Fatalf("patch visibility: %d", patch)
	}
	if anon := f.get(path, ""); anon.Code != 200 {
		t.Fatalf("public cover not readable: %d", anon.Code)
	}
	// 不存在的素材名不能因为"形状对"就 200。
	for _, name := range []string{
		"00000000000000000000000000000000.png",
		"deadbeef.png",
	} {
		if w := f.get("/api/v1/games/demo-game/assets/"+name, f.token); w.Code != 404 {
			t.Fatalf("asset %q: got %d want 404", name, w.Code)
		}
	}
	// 路径穿越必须挡住。百分号编码的形式会真的进到 handler（`..` 的明文形式会被
	// net/http 的 mux 先规整成 301），所以这里专门用编码形式打自己的防线。
	if w := f.get("/api/v1/games/demo-game/assets/%2e%2e%2f%2e%2e%2fetc%2fpasswd", f.token); w.Code != 404 {
		t.Fatalf("encoded traversal: got %d want 404", w.Code)
	}
	// 明文形式至少不能被当成有效素材发出去（mux 会先 301 规整）。
	if w := f.get("/api/v1/games/demo-game/assets/../../../../etc/passwd", f.token); w.Code == 200 {
		t.Fatal("plain traversal was served")
	}
}

func (f *uploadFixture) patchVisibility(t *testing.T, id string) int {
	t.Helper()
	r := httptest.NewRequest("PATCH", "/api/v1/games/"+id, bytes.NewReader([]byte(`{"visibility":"public"}`)))
	r.Header.Set("Authorization", "Bearer "+f.token)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w.Code
}

func TestManifestCarriesBundleFormatAndCover(t *testing.T) {
	f := newUploadFixture(t)
	if w := f.put(t, "demo-game", validBundle(t, nil), ""); w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if code := f.patchVisibility(t, "demo-game"); code != 200 {
		t.Fatalf("patch visibility: %d", code)
	}
	w := f.get("/gamesource/v1/index.json", "")
	if w.Code != 200 {
		t.Fatalf("manifest: %d", w.Code)
	}
	var manifest struct {
		Games []map[string]any `json:"games"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Games) != 1 {
		t.Fatalf("manifest games: %d", len(manifest.Games))
	}
	game := manifest.Games[0]
	pack, _ := game["package"].(map[string]any)
	if pack["format"] != "game/1" {
		t.Fatalf("manifest format: %v", pack)
	}
	if game["cover"] == nil || game["coverSha256"] == nil {
		t.Fatalf("manifest cover missing: %v", game)
	}
}

// 配上对象存储之后，**裸 pck 上传**必须成功——这条覆盖的是真正打挂线上 15 款的那个 bug。
//
// 当时的故障链：裸包路径把刚收上来的临时文件直接交给 OSS 上传，而 http.NewRequest 会把
// io.ReadCloser 当成 req.Body、http 客户端请求结束后顺手关掉它；紧接着的 f.Sync() 报
// "file already closed"，接口返回 503。容器路径"看起来没事"，只是因为那时上传用的是
// 另一个句柄——所以只测容器是抓不到的，必须两条路径都打。
func TestUploadWithObjectStorageSucceedsForBarePCK(t *testing.T) {
	f := newUploadFixture(t)
	f.s.OSS = testOSSClient(t)

	meta := base64.StdEncoding.EncodeToString([]byte(`{"title":"带对象存储的裸包","engine":{"name":"godot","version":"4.7"}}`))
	w := f.put(t, "oss-bare-pck", validEntry(), meta)
	if w.Code != 201 {
		t.Fatalf("裸包上传（配 OSS）应为 201，实际 %d: %s", w.Code, w.Body.String())
	}
	// 容器路径同样要能过（它用的是另一个句柄，是这次没被打挂的那条）。
	w2 := f.put(t, "demo-game", validBundle(t, nil), "")
	if w2.Code != 201 {
		t.Fatalf("容器上传（配 OSS）应为 201，实际 %d: %s", w2.Code, w2.Body.String())
	}
}
