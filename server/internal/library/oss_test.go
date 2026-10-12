package library

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// 上传过程中的 OSS Put **不能关掉调用方的文件句柄**。
//
// 踩过的坑：http.NewRequest 遇到 io.ReadCloser 会直接把它当 req.Body，而 http 客户端
// 在请求结束后会 Close req.Body。于是 put() 把刚收上来的临时文件交给 OSS 上传之后，
// 紧接着的 f.Sync() 报 "file already closed"，裸 pck 上传全线 503。
// 容器路径之所以"看起来正常"，只是因为那时上传用的是另一个句柄。
// testOSSClient 是一个**真实的** OSS 客户端，只是把所有连接都引到本地假服务器。
//
// 故意不用假的 AssetStore 实现：这次打挂线上 15 款上传的 bug 就藏在
// "http 客户端替调用方关闭请求体"这个行为里，假实现永远不会复现它。
func testOSSClient(t *testing.T) *OSSClient {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, err := NewOSSClient(OSSConfig{Bucket: "b", Endpoint: "oss.example", AccessKeyID: "i", AccessKeySecret: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// URL 里的 host 是合成出来的（b.oss.example），解析不了，所以直接改拨号目标。
	c.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	return c
}

func TestPutObjectStreamKeepsCallersFileOpen(t *testing.T) {
	c := testOSSClient(t)
	f, err := os.CreateTemp(t.TempDir(), "p-*.part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("hello"); err != nil {
		t.Fatal(err)
	}
	// 真实路径在这里会 Seek(0) 再上传；不 seek 读到的是 EOF。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	if err := c.PutObjectStream("k.pck", f, 5, "application/octet-stream"); err != nil {
		t.Fatalf("put 失败: %v", err)
	}
	// 这一句就是回归点：上传之后调用方必须还能用自己的句柄。
	if err := f.Sync(); err != nil {
		t.Fatalf("上传把调用方的文件句柄关掉了: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("上传之后句柄应仍可用: %v", err)
	}
}

func testOSS(t *testing.T) (*OSSClient, OSSConfig) {
	t.Helper()
	cfg := OSSConfig{
		Bucket: "gamestop-assets", Endpoint: "oss-cn-hangzhou.aliyuncs.com",
		AccessKeyID: "AKID", AccessKeySecret: "SECRET",
		Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	c, err := NewOSSClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, cfg
}

func TestOSSConfigValidation(t *testing.T) {
	base := OSSConfig{Bucket: "b", Endpoint: "e", AccessKeyID: "i", AccessKeySecret: "s"}
	c, err := NewOSSClient(base)
	if err != nil {
		t.Fatalf("最小配置应可用: %v", err)
	}
	// 默认值必须补齐，否则 key 会变成 "gamesfoo/..." 这种少一个斜杠的脏路径。
	if c.Prefix() != DefaultOSSPrefix || !strings.HasSuffix(c.Prefix(), "/") {
		t.Fatalf("prefix 默认值不对: %q", c.Prefix())
	}
	for _, bad := range []OSSConfig{
		{Bucket: "", Endpoint: "e", AccessKeyID: "i", AccessKeySecret: "s"},
		{Bucket: "b", Endpoint: "", AccessKeyID: "i", AccessKeySecret: "s"},
		{Bucket: "b", Endpoint: "e", AccessKeyID: "", AccessKeySecret: "s"},
		{Bucket: "b", Endpoint: "e", AccessKeyID: "i", AccessKeySecret: ""},
		// bucket 里塞路径、endpoint 里塞协议，都是"看起来能用其实会拼出坏 URL"的写法。
		{Bucket: "b/x", Endpoint: "e", AccessKeyID: "i", AccessKeySecret: "s"},
		{Bucket: "b", Endpoint: "https://e", AccessKeyID: "i", AccessKeySecret: "s"},
	} {
		if _, err := NewOSSClient(bad); err == nil {
			t.Fatalf("应该拒绝: %+v", bad)
		}
	}
	// 没配任何 OSS 变量时必须返回 nil（退回本地磁盘），而不是报错或给个半成品客户端。
	for _, k := range []string{"OSS_BUCKET", "OSS_ENDPOINT", "OSS_ACCESS_KEY_ID", "OSS_ACCESS_KEY_SECRET"} {
		t.Setenv(k, "")
	}
	c, err = OSSFromEnv()
	if err != nil || c != nil {
		t.Fatalf("未配置时应返回 nil,nil，实际 %v %v", c, err)
	}
}

func TestOSSAssetKeyIsContentAddressed(t *testing.T) {
	c, _ := testOSS(t)
	sha := strings.Repeat("ab", 32)
	key := c.AssetKey("find-1-to-49", "cover", sha, ".png")
	want := "games/find-1-to-49/" + strings.Repeat("ab", 8) + "/cover.png"
	if key != want {
		t.Fatalf("key 形状不对:\n got %q\nwant %q", key, want)
	}
	// 内容变 → key 变（否则会出现"URL 没变内容变了"的缓存不一致）。
	if key == c.AssetKey("find-1-to-49", "cover", strings.Repeat("cd", 32), ".png") {
		t.Fatal("不同内容必须落到不同 key")
	}
	// 同一个内容 → 同一个 key（重传同样的图不该产生孤儿对象）。
	if key != c.AssetKey("find-1-to-49", "cover", sha, ".png") {
		t.Fatal("同样内容应落在同一个 key")
	}
	if got := c.AssetKey("g", AssetRole(0), sha, ".png"); !strings.HasSuffix(got, "/cover.png") {
		t.Fatalf("cover 角色不对: %q", got)
	}
	if got := c.AssetKey("g", AssetRole(2), sha, ".jpg"); !strings.HasSuffix(got, "/shot-2.jpg") {
		t.Fatalf("截图角色不对: %q", got)
	}
}

func TestOSSSignedURLCarriesProcessAndExpiry(t *testing.T) {
	c, _ := testOSS(t)
	key := c.AssetKey("g", "cover", strings.Repeat("ab", 32), ".png")
	raw := c.SignedGetURL(key, CoverProcess, time.Unix(1700000000, 0).UTC())
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "gamestop-assets.oss-cn-hangzhou.aliyuncs.com" {
		t.Fatalf("host 不对: %s", u.Host)
	}
	if !strings.HasPrefix(u.Path, "/games/g/") {
		t.Fatalf("path 不对: %s", u.Path)
	}
	q := u.Query()
	if q.Get("x-oss-process") != CoverProcess {
		t.Fatalf("预处理参数丢了: %q", q.Get("x-oss-process"))
	}
	if q.Get("OSSAccessKeyId") != "AKID" {
		t.Fatal("缺 OSSAccessKeyId")
	}
	// 1 小时的默认有效期。
	if got, want := q.Get("Expires"), "1700003600"; got != want {
		t.Fatalf("Expires=%s want=%s", got, want)
	}
	if len(q.Get("Signature")) < 20 {
		t.Fatal("签名太短，八成没算")
	}
	// 签名必须覆盖 x-oss-process：否则改了预处理参数签名依然有效。
	other := c.SignedGetURL(key, ShotProcess, time.Unix(1700000000, 0).UTC())
	otherURL, err := url.Parse(other)
	if err != nil {
		t.Fatal(err)
	}
	// 注意：要比**签名本身**。URL 的查询参数按字典序排列，`&Signature=` 之前那段
	// 并不包含 x-oss-process，拿它比会永远相等（这里踩过）。
	if q.Get("Signature") == otherURL.Query().Get("Signature") {
		t.Fatal("签名没有覆盖 x-oss-process：改了预处理参数签名却没变")
	}
	// 签名是 base64，本来就会带 +/= 这些字符；能被 url.Parse 原样解回来就够了
	// （上面 q.Get 已经是解码后的值）。
}

func TestOSSSigningIsDeterministicAndKeySensitive(t *testing.T) {
	c, cfg := testOSS(t)
	now := time.Unix(1700000000, 0).UTC()
	key := "games/g/x/cover.png"
	a := c.SignedGetURL(key, CoverProcess, now)
	b := c.SignedGetURL(key, CoverProcess, now)
	if a != b {
		t.Fatal("同样的输入必须产出同样的签名")
	}
	// 换密钥 → 签名必须变（防止把 secret 拼错却"看起来能跑"）。
	cfg.AccessKeySecret = "OTHER"
	c2, _ := NewOSSClient(cfg)
	if c2.SignedGetURL(key, CoverProcess, now) == a {
		t.Fatal("密钥不同签名却相同")
	}
	// 换对象 → 签名必须变。
	if c.SignedGetURL("games/g/y/cover.png", CoverProcess, now) == a {
		t.Fatal("对象不同签名却相同")
	}
}

func TestEscapePathKeepsSlashes(t *testing.T) {
	// 中文/空格要转义，但路径分隔符不能被吃掉，否则 key 层级就塌了。
	got := escapePath("games/中文 名/cover.png")
	if strings.Count(got, "/") != 2 {
		t.Fatalf("分隔符数量不对: %q", got)
	}
	if strings.Contains(got, " ") {
		t.Fatalf("空格未转义: %q", got)
	}
}
