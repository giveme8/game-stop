package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// `.game` 容器的入库落盘：校验、抽素材、算哈希。契约见 docs/api/game-bundle-format.md。
//
// 注意两件事：
//  1. 素材按**服务端生成的名字**落盘，绝不采用包内文件名（沿用"存储使用服务端生成的路径"的硬限制）。
//  2. 任何一步失败都要把已经落盘的素材删干净，不能留下半成品。

const assetsDirName = "assets"

// 素材文件名形如 <32 位十六进制><扩展名>；取用时必须按这个形状校验，
// 否则一个来自数据库的字符串就能变成路径穿越。
var assetNamePattern = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|webp)$`)

// GameAsset 是入库后素材的身份。
// OSS 形态用 Key（结构化对象路径），本地磁盘形态用 File（服务端生成的文件名），
// 两者只会有一个非空——取图时按这个判断，不要猜。
type GameAsset struct {
	File   string `json:"file"`
	Key    string `json:"key,omitempty"`
	SHA256 string `json:"sha256"`
	Mime   string `json:"mime"`
}

type GameAssets struct {
	Cover       *GameAsset  `json:"cover,omitempty"`
	Screenshots []GameAsset `json:"screenshots,omitempty"`
}

var bundleMime = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".webp": "image/webp"}

// ingestedBundle 是一次成功入库的全部产物。
type ingestedBundle struct {
	Metadata Metadata
	Source   BundleSource
	Assets   GameAssets
	// 从容器里解出来的入口 pck 落在临时文件里。**OSS 上存的是它，不是容器本身**：
	// `.game` 是上传方与平台之间的提交协议，平台把它拆成"图片 + pck"再对外分发。
	// 调用方负责上传完之后清理 EntryPath。
	EntryPath string
	EntrySize int64
	// 入口 pck 自己的 sha256：OSS 上的对象名与清单里的校验值都用它。
	EntrySHA256 string
}

// ingestBundle 只做校验与素材落盘，**不动数据库**：调用方负责在数据库写入失败时
// 用返回的 cleanup 删掉素材。
func (s *Server) ingestBundle(file *os.File, size int64, id string) (ingestedBundle, func(), error) {
	var out ingestedBundle
	noop := func() {}
	plan, err := InspectBundle(file, size, id)
	if err != nil {
		return out, noop, err
	}
	// 入口必须是一个合法 pck，且版本与 game.json 一致——这是"两处元数据"合并成一处之后
	// 唯一的交叉校验点。
	rc, err := plan.OpenEntry()
	if err != nil {
		return out, noop, errBundle
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(s.DataDir, "entry-*.pck")
	if err != nil {
		return out, noop, err
	}
	tmpName := tmp.Name()
	defer tmp.Close()
	// 成功时把文件**所有权交给调用方**（它要拿去上传），只有失败才在这里删。
	// 早先无条件 defer os.Remove 会让调用方拿到一个已被删掉的路径。
	handedOver := false
	defer func() {
		if !handedOver {
			_ = os.Remove(tmpName)
		}
	}()
	entryHash := sha256.New()
	n, err := CopyLimited(io.MultiWriter(tmp, entryHash), rc, MaxPackageBytes)
	if err != nil {
		return out, noop, errBundle
	}
	version, err := InspectPCK(tmp, n)
	if err != nil {
		return out, noop, errBundle
	}
	if version != plan.Meta.Engine.Version {
		return out, noop, fmt.Errorf("%w: engine version mismatch", errBundle)
	}
	// 校验通过后把临时文件留下来给调用方上传——否则这里白解一次，稍后还得再解。
	if err := tmp.Sync(); err != nil {
		return out, noop, err
	}
	out.EntryPath = tmpName
	out.EntrySize = n
	out.EntrySHA256 = hex.EncodeToString(entryHash.Sum(nil))
	handedOver = true

	assets, cleanup, err := s.storeAssets(plan, id)
	if err != nil {
		return out, noop, err
	}
	out.Metadata = plan.PlatformMetadata()
	out.Source = plan.Meta.Source
	out.Assets = assets
	return out, cleanup, nil
}

// storeAssets 落封面与截图：配了 OSS 就进 OSS，否则落本地磁盘。
// 返回的 cleanup 在入库失败时把这次已经写出去的东西删干净（两个地方都要清）。
func (s *Server) storeAssets(plan *BundlePlan, gameID string) (GameAssets, func(), error) {
	var out GameAssets
	stored := make([]GameAsset, 0, len(plan.Assets))
	cleanup := func() {
		for _, a := range stored {
			s.dropAsset(a)
		}
	}
	for i, spec := range plan.Assets {
		asset, err := s.storeAsset(plan, spec, AssetRole(i), gameID)
		if err != nil {
			cleanup()
			return GameAssets{}, func() {}, err
		}
		stored = append(stored, asset)
		if i == 0 {
			cover := asset
			out.Cover = &cover
			continue
		}
		out.Screenshots = append(out.Screenshots, asset)
	}
	if out.Cover == nil {
		cleanup()
		return GameAssets{}, func() {}, errBundle
	}
	return out, cleanup, nil
}

// dropAsset 删掉一份已落盘的素材（OSS 或本地），用于失败回滚。
func (s *Server) dropAsset(a GameAsset) {
	if a.Key != "" && s.OSS != nil {
		_ = s.OSS.DeleteObject(a.Key)
		return
	}
	if a.File != "" {
		_ = os.Remove(filepath.Join(s.DataDir, assetsDirName, a.File))
	}
}

// storeAsset 落一份素材。key/文件名都带素材自己的 sha（内容寻址），所以必须先读完
// 才能决定放哪儿——这是唯一把单张图放进内存的地方，上限就是素材自身的体积（≤8 MiB）。
func (s *Server) storeAsset(plan *BundlePlan, spec BundleAsset, role, gameID string) (GameAsset, error) {
	rc, err := plan.OpenAsset(spec)
	if err != nil {
		return GameAsset{}, errBundle
	}
	defer rc.Close()
	var buf bytes.Buffer
	// 体积上限以容器声明为准，同时用 CopyLimited 兜住"声明撒谎"的情况。
	if _, err := CopyLimited(&buf, rc, spec.Max); err != nil {
		return GameAsset{}, errBundle
	}
	body := buf.Bytes()
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	mime := bundleMime[spec.Ext]

	if s.OSS != nil {
		key := s.OSS.AssetKey(gameID, role, digest, spec.Ext)
		if err := s.OSS.PutObject(key, body, mime); err != nil {
			return GameAsset{}, err
		}
		return GameAsset{Key: key, SHA256: digest, Mime: mime}, nil
	}

	dir := filepath.Join(s.DataDir, assetsDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return GameAsset{}, err
	}
	name, err := randomHex(16)
	if err != nil {
		return GameAsset{}, err
	}
	name += spec.Ext
	dest := filepath.Join(dir, name)
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return GameAsset{}, err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		os.Remove(dest)
		return GameAsset{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(dest)
		return GameAsset{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(dest)
		return GameAsset{}, err
	}
	return GameAsset{File: name, SHA256: digest, Mime: mime}, nil
}

// asset 提供封面与截图，可见性规则与包下载完全一致（私有游戏对匿名一律 404）。
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	g, ok := s.readable(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var assets GameAssets
	if !assetNamePattern.MatchString(name) || json.Unmarshal([]byte(g.Assets), &assets) != nil {
		fail(w, 404, "asset not found")
		return
	}
	mime := ""
	for _, candidate := range assetList(assets) {
		if candidate.File == name {
			mime = candidate.Mime
			break
		}
	}
	if mime == "" {
		// 记录里没有这张图：不能因为"文件恰好在磁盘上"就发出去。
		fail(w, 404, "asset not found")
		return
	}
	f, err := os.Open(filepath.Join(s.DataDir, assetsDirName, name))
	if err != nil {
		fail(w, 404, "asset not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		fail(w, 404, "asset not found")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func assetList(a GameAssets) []GameAsset {
	list := make([]GameAsset, 0, len(a.Screenshots)+1)
	if a.Cover != nil {
		list = append(list, *a.Cover)
	}
	return append(list, a.Screenshots...)
}

func bundleAssetURL(base, gameID, file string) string {
	if file == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/api/v1/games/" + gameID + "/" + assetsDirName + "/" + file
}

// assetURL 给出这张图**对外**的地址。
//
// OSS 形态：签发一个带预处理的临时 URL（桶是私有的，没有别的读法）。
// 本地形态（迁移前入库的老记录）：仍然走平台自己的素材端点。
func (s *Server) assetURL(gameID string, a GameAsset, process string) string {
	if a.Key != "" && s.OSS != nil {
		return s.OSS.SignedGetURLNow(a.Key, process)
	}
	return bundleAssetURL(s.BaseURL, gameID, a.File)
}
