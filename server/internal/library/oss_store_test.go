package library

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeStore 记录所有调用，用来断言"素材到底去了哪儿"。
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	puts    []string
	dels    []string
	failPut error
	// failAfter > 0 时，前 failAfter 次上传成功、之后才失败——用来验证"传了一半再挂"
	// 时已经传上去的对象会被删掉（第一张就失败的话根本没东西可回滚，等于没测）。
	failAfter int
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, types: map[string]string{}}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (f *fakeStore) AssetKey(gameID, role, sha256, ext string) string {
	d := sha256
	if len(d) > 16 {
		d = d[:16]
	}
	return "games/" + gameID + "/" + d + "/" + role + ext
}

func (f *fakeStore) PackageKey(gameID, sha256, ext string) string {
	return "packages/" + gameID + "/" + sha256 + ext
}

func (f *fakeStore) PutObjectStream(key string, r io.Reader, size int64, contentType string) error {
	body, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return err
	}
	if int64(len(body)) != size {
		return fmt.Errorf("流式上传的实际长度 %d 与声明的 %d 不一致", len(body), size)
	}
	return f.PutObject(key, body, contentType)
}

func (f *fakeStore) PutObject(key string, body []byte, contentType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPut != nil && (f.failAfter == 0 || len(f.puts) >= f.failAfter) {
		return f.failPut
	}
	f.objects[key] = append([]byte(nil), body...)
	f.types[key] = contentType
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeStore) DeleteObject(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.dels = append(f.dels, key)
	return nil
}

func (f *fakeStore) SignedGetURLNow(key, process string) string {
	return "https://fake-oss/" + key + "?process=" + process + "&signed=1"
}

func (f *fakeStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

func (f *fakeStore) delCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dels)
}

func withStore(t *testing.T, f *fakeStore) *uploadFixture {
	t.Helper()
	fx := newUploadFixture(t)
	fx.s.OSS = f
	return fx
}

func TestBundleAssetsGoToObjectStorage(t *testing.T) {
	f := newFakeStore()
	fx := withStore(t, f)
	raw := validBundle(t, func(m map[string]any) {
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	}, zipEntry{name: "assets/shot-1.png", data: []byte(testPNG + "shot")})

	w := fx.put(t, "demo-game", raw, "")
	if w.Code != 201 {
		t.Fatalf("上传失败: %d %s", w.Code, w.Body.String())
	}
	// 三个对象：封面 + 截图 + 包。
	if f.putCount() != 3 {
		t.Fatalf("应有 封面+截图+包 三个对象，实际上传了 %d 个: %v", f.putCount(), f.puts)
	}
	var packageKeys []string
	for _, key := range f.puts {
		if strings.HasPrefix(key, "packages/") {
			packageKeys = append(packageKeys, key)
			continue
		}
		if !strings.HasPrefix(key, "games/demo-game/") {
			t.Fatalf("key 前缀不对: %s", key)
		}
		if !strings.HasSuffix(key, "/cover.png") && !strings.HasSuffix(key, "/shot-1.png") {
			t.Fatalf("key 角色不对: %s", key)
		}
		if f.types[key] != "image/png" {
			t.Fatalf("Content-Type 不对: %s -> %s", key, f.types[key])
		}
	}
	if len(packageKeys) != 1 {
		t.Fatalf("应恰好有一个包对象，实际 %v", packageKeys)
	}
	// 这一条是本次改动的核心：OSS 上存的是**解出来的 pck**，不是 .game 容器。
	// 容器是 zip（PK\x03\x04），pck 是 GDPC；体积也不同（容器多包了 game.json 与封面）。
	pkgBody := f.objects[packageKeys[0]]
	if len(pkgBody) < 4 || string(pkgBody[:4]) != "GDPC" {
		t.Fatalf("包对象不是 pck（前四字节 %q）", string(pkgBody[:minInt(4, len(pkgBody))]))
	}
	if strings.HasSuffix(packageKeys[0], ".game") {
		t.Fatalf("包对象不该是容器: %s", packageKeys[0])
	}
	if !strings.Contains(packageKeys[0], "/"+sha256Hex(pkgBody)+".pck") {
		t.Fatalf("包对象名应带 pck 自己的 sha256: %s", packageKeys[0])
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	// 清单必须按 pck 描述：sha/大小与容器不同，客户端拿它校验下载到的东西。
	pack, _ := body["package"].(map[string]any)
	if pack["format"] != "pck" {
		t.Fatalf("对外分发的是 pck，format 应为 pck: %v", pack["format"])
	}
	if int64(pack["size"].(float64)) != int64(len(pkgBody)) {
		t.Fatalf("清单里的 size(%v) 应等于 pck 的体积(%d)", pack["size"], len(pkgBody))
	}
	if pack["sha256"] != sha256Hex(pkgBody) {
		t.Fatalf("清单里的 sha256 应是 pck 的")
	}
	// 上传回执必须描述"收到的字节"（容器）。发布工具手里只有容器，拿 pck 的 sha 去对账
	// 永远不等——这正是上线时撞到的"回执不符"。
	received, _ := body["received"].(map[string]any)
	if received == nil {
		t.Fatalf("缺少 received 回执")
	}
	if received["sha256"] != sha256Hex(raw) || int64(received["size"].(float64)) != int64(len(raw)) {
		t.Fatalf("received 应描述上传的容器: %v（容器 %d 字节）", received, len(raw))
	}
	if received["sha256"] == pack["sha256"] {
		t.Fatalf("容器与 pck 的 sha 不该相同，否则这条断言没意义")
	}
	if !strings.Contains(fmt.Sprint(pack["url"]), "signed=1") {
		t.Fatalf("包的 URL 应是签名 URL: %v", pack["url"])
	}
	cover, _ := body["cover"].(string)
	if !strings.Contains(cover, "fake-oss") || !strings.Contains(cover, "signed=1") {
		t.Fatalf("cover 应是签名 URL: %q；响应全文=%s", cover, w.Body.String())
	}
	if !strings.Contains(cover, "webp") {
		t.Fatalf("封面 URL 应带 webp 预处理: %s", cover)
	}
	shots, _ := body["screenshots"].([]any)
	if len(shots) != 1 || !strings.Contains(fmt.Sprint(shots[0]), "signed=1") {
		t.Fatalf("截图也应是签名 URL: %v", body["screenshots"])
	}
	assertNoLocalAssets(t, fx)
}

// 上传失败必须回滚：已经传上去的对象要删掉，不能留下没人引用的孤儿。
func TestBundleAssetUploadFailureRollsBack(t *testing.T) {
	f := newFakeStore()
	f.failPut = errors.New("oss 挂了")
	f.failAfter = 1 // 封面传成功、截图失败
	fx := withStore(t, f)
	raw := validBundle(t, func(m map[string]any) {
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	}, zipEntry{name: "assets/shot-1.png", data: []byte(testPNG + "shot")})
	w := fx.put(t, "demo-game", raw, "")
	if w.Code != 400 && w.Code != 503 {
		t.Fatalf("上传失败应报错，实际 %d %s", w.Code, w.Body.String())
	}
	// 关键：先成功传上去的那张必须被删掉，否则桶里会攒下一堆没人引用的孤儿图。
	if f.delCount() != 1 {
		t.Fatalf("应回滚已上传的对象，实际删除 %d 个（上传成功 %d 个）", f.delCount(), f.putCount())
	}
	var count int64
	if err := fx.db.Model(&Game{}).Where("id = ?", "demo-game").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("失败的上传不该留记录: %d %v", count, err)
	}
	assertNoLocalAssets(t, fx)
}

// 容器校验失败（比 OSS 更早的那一层）不能碰对象存储。
func TestRejectedBundleNeverTouchesStorage(t *testing.T) {
	f := newFakeStore()
	fx := withStore(t, f)
	raw := validBundle(t, nil, zipEntry{name: "content/evil.exe", data: []byte("MZ")})
	if w := fx.put(t, "demo-game", raw, ""); w.Code != 400 {
		t.Fatalf("非法容器应被拒: %d", w.Code)
	}
	if f.putCount() != 0 {
		t.Fatalf("非法容器不该上传任何对象，实际 %d 个", f.putCount())
	}
}

func assertNoLocalAssets(t *testing.T, fx *uploadFixture) {
	t.Helper()
	var games []Game
	if err := fx.db.Find(&games).Error; err != nil {
		t.Fatal(err)
	}
	for _, g := range games {
		var assets GameAssets
		if json.Unmarshal([]byte(g.Assets), &assets) != nil {
			continue
		}
		for _, a := range assetList(assets) {
			if a.File != "" {
				t.Fatalf("配了 OSS 却仍写了本地文件: %s", a.File)
			}
		}
	}
}

// 重传（走 UPDATE 分支）必须把新列也写进去。
//
// 更新分支的列清单是写死的，新字段忘了加就会**静默不落库**——真踩过：包明明传上了
// OSS，记录里 package_key 却是空的，于是对外给出平台端点地址，下载还返回一个荒谬的
// Content-Length（服务端去打开一个空文件名的路径）。
func TestReuploadPersistsObjectStorageColumns(t *testing.T) {
	f := newFakeStore()
	fx := withStore(t, f)

	// 先造一条**没有对象存储**的老记录（等价于"这个字段还不存在时建的游戏"），
	// 否则 create 分支会把新列都写满，UPDATE 漏列也看不出来——那样这条测试就是假的。
	fx.s.OSS = nil
	if w := fx.put(t, "demo-game", validBundle(t, nil), ""); w.Code != 201 {
		t.Fatalf("首次上传应 201，实际 %d %s", w.Code, w.Body.String())
	}
	var before Game
	_ = fx.db.Where("id = ?", "demo-game").First(&before).Error
	if before.PackageKey != "" {
		t.Fatal("前提不成立：这条老记录本该没有 package_key")
	}

	// 再传一次，这次配了 OSS，走的是 UPDATE 分支。
	fx.s.OSS = f
	w := fx.put(t, "demo-game", validBundle(t, nil), "")
	if w.Code != 200 {
		t.Fatalf("重传应 200，实际 %d %s", w.Code, w.Body.String())
	}
	var g Game
	if err := fx.db.Where("id = ?", "demo-game").First(&g).Error; err != nil {
		t.Fatal(err)
	}
	if g.PackageKey == "" {
		t.Fatal("重传后 package_key 丢了：包在 OSS 上，记录却以为在本地")
	}
	if g.Filename != "" {
		t.Fatalf("OSS 形态不该有本地文件名，实际 %q", g.Filename)
	}
	if g.ReceivedSHA256 == "" || g.ReceivedSize == 0 {
		t.Fatal("重传后上传回执丢了")
	}
	// 对外地址必须指向 OSS（不是平台端点）。
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	pack, _ := body["package"].(map[string]any)
	if !strings.Contains(fmt.Sprint(pack["url"]), "fake-oss") {
		t.Fatalf("重传后包地址应指向对象存储，实际 %v", pack["url"])
	}
}
