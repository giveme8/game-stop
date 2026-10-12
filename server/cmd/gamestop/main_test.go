package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectUnsafeOrigins(t *testing.T) {
	for _, s := range []string{"http://example.com", "https://u:p@example.com", "https://example.com/path", "https://example.com?token=secret", "https://example.com/#x"} {
		if _, err := newClient(s, ""); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}

func TestDownloadIntegrityAndNoOverwrite(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			payload := []byte("verified pck content")
			h := sha256.Sum256(payload)
			digest := hex.EncodeToString(h[:])
			if bad {
				digest = hex.EncodeToString(make([]byte, 32))
			}
			var self string
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("missing bearer")
				}
				if r.URL.Path == "/api/v1/games/demo" {
					// 同源包地址：走平台自己的端点，带 Bearer（老记录/未配对象存储的部署）。
					fmt.Fprintf(w, `{"id":"demo","package":{"url":"%s/api/v1/games/demo/package.pck","size":%d,"sha256":"%s"}}`, self, len(payload), digest)
					return
				}
				w.Write(payload)
			}))
			defer ts.Close()
			self = ts.URL
			c, _ := newClient(ts.URL, "test-secret")
			c.http = ts.Client()
			dir := t.TempDir()
			out := filepath.Join(dir, "demo.pck")
			err := c.download("demo", out)
			if bad {
				if err == nil {
					t.Fatal("bad checksum accepted")
				}
				if _, err = os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("corrupt output published")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = c.download("demo", out); err == nil {
					t.Fatal("overwrote destination")
				}
				b, _ := os.ReadFile(out)
				if string(b) != string(payload) {
					t.Fatal("content changed")
				}
			}
			files, _ := filepath.Glob(filepath.Join(dir, "*.part"))
			if len(files) > 0 {
				t.Fatal("left partial files")
			}
		})
	}
}

func TestRedirectDoesNotForwardToken(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/leak", http.StatusFound)
	}))
	defer ts.Close()
	c, _ := newClient(ts.URL, "secret")
	c.http.Transport = ts.Client().Transport
	if _, err := c.jsonRequest("GET", "/api/v1/games", nil); err == nil {
		t.Fatal("redirect accepted")
	}
}

// 包现在多数存在对象存储上，清单里的 package.url 会指向另一个域名。
// 规矩保住的是"**凭据不出同源**"，而不是"绝不跨源取包"——后者已经做不到，
// 因为包本来就不在平台机器上了。跨源时只认 https 与清单里的 size+sha256。
func TestDownloadFromObjectStorageWithoutCredentials(t *testing.T) {
	payload := []byte("pck bytes stored in object storage")
	sum := sha256.Sum256(payload)

	var sawAuth []string
	store := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = append(sawAuth, r.Header.Get("Authorization"))
		w.Write(payload)
	}))
	defer store.Close()

	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"demo","package":{"url":"%s/packages/demo/%s.pck","size":%d,"sha256":"%s"}}`,
			store.URL, hex.EncodeToString(sum[:]), len(payload), hex.EncodeToString(sum[:]))
	}))
	defer api.Close()

	c, _ := newClient(api.URL, "test-secret")
	c.http = api.Client() // 两个测试服务用的是同一套自签证书

	out := filepath.Join(t.TempDir(), "demo.pck")
	if err := c.download("demo", out); err != nil {
		t.Fatalf("跨源下载失败: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("落盘内容不对: %v", err)
	}
	// 关键断言：对第三方**一个凭据都没发**。
	if len(sawAuth) == 0 {
		t.Fatal("对象存储没被访问到")
	}
	for _, h := range sawAuth {
		if h != "" {
			t.Fatalf("把凭据发给了第三方: %q", h)
		}
	}
}

// 明文地址一律拒绝：清单被改成 http 时不能悄悄降级传输。
func TestDownloadRejectsPlaintextPackageURL(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"demo","package":{"url":"http://object.example/demo.pck","size":10,"sha256":"%s"}}`,
			strings.Repeat("a", 64))
	}))
	defer api.Close()
	c, _ := newClient(api.URL, "test-secret")
	c.http = api.Client()
	if err := c.download("demo", filepath.Join(t.TempDir(), "demo.pck")); err == nil {
		t.Fatal("明文 package.url 必须被拒")
	}
}

// 服务端说"为什么"时必须把它带出来。
//
// 以前只回显状态码，503 在用户眼里就是光秃秃一个 "503"——连是我们返回的哪一种
// 503（对象存储不可用 / 存储不可用 / 上传槽满）都分不出来，我只能靠数响应体
// 字节数去反推。这条把"要带出 error 文案"钉住。
func TestServerErrorIsSurfaced(t *testing.T) {
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"error\":\"storage unavailable\"}\n"))
	}))
	defer api.Close()
	c, _ := newClient(api.URL, "test-secret")
	c.http = api.Client()
	_, err := c.jsonRequest("GET", "/api/v1/games", nil)
	if err == nil {
		t.Fatal("503 应当报错")
	}
	if !strings.Contains(err.Error(), "storage unavailable") {
		t.Fatalf("没有带出服务端的说明: %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Fatalf("状态码也得留着: %v", err)
	}
	// 非 JSON 或没有 error 字段时退回通用提示，不能把一坨 HTML 糊到用户脸上。
	if got := serverError(strings.NewReader("<html>502 Bad Gateway</html>")); got != "" {
		t.Fatalf("非 JSON 不该被当说明: %q", got)
	}
}
