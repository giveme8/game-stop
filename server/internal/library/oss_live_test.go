package library

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 真桶验证。**默认跳过**：CI 与本地没配 OSS_* 时它不会跑，也不会失败。
//
// 为什么必须有这一条：URL 签名（尤其是要把 x-oss-process 这个子资源算进
// CanonicalizedResource）是最容易"看起来对、实际 SignatureDoesNotMatch"的地方，
// 单测只能保证形状，只有真服务能判对错。
//
// 仅在专用测试桶环境运行：配置 OSS_* 后执行 go test -run TestOSSLiveRoundTrip。
// 本测试会写入和删除对象，绝不要使用生产凭据；配置项见 ../../../deploy/README.md。
func TestOSSLiveRoundTrip(t *testing.T) {
	c, err := OSSFromEnv()
	if err != nil {
		t.Fatalf("OSS 配置有问题: %v", err)
	}
	if c == nil {
		t.Skip("未配置 OSS_*，跳过真桶验证")
	}

	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{R: 224, G: 36, B: 58, A: 255})
		}
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()

	digest := sha256.Sum256(body)
	sum := hex.EncodeToString(digest[:])
	key := c.AssetKey("oss-live-check", "cover", sum, ".png")
	// 先清掉上次可能的残留，保证这次测的是"刚上传的那个对象"。
	if err := c.DeleteObject(key); err != nil {
		t.Fatalf("预清理失败: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(key) })

	if err := c.PutObject(key, body, "image/png"); err != nil {
		t.Fatalf("上传失败: %v", err)
	}

	now := time.Now()
	client := &http.Client{Timeout: 30 * time.Second}

	t.Run("原图可取", func(t *testing.T) {
		resp, err := client.Get(c.SignedGetURL(key, "", now))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		got, _ := io.ReadAll(resp.Body)
		if !bytes.Equal(got, body) {
			t.Fatalf("取回的字节与上传的不一致: %d vs %d", len(got), len(body))
		}
	})

	t.Run("带预处理的签名 URL 生效", func(t *testing.T) {
		resp, err := client.Get(c.SignedGetURL(key, CoverProcess, now))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			got, _ := io.ReadAll(resp.Body)
			if len(got) > 300 {
				got = got[:300]
			}
			t.Fatalf("status=%d body=%s", resp.StatusCode, string(got))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "webp") {
			t.Fatalf("预处理没有转成 webp，Content-Type=%q", ct)
		}
	})

	t.Run("篡改签名必须被拒", func(t *testing.T) {
		bad := strings.Replace(c.SignedGetURL(key, "", now), "Signature=", "Signature=x", 1)
		resp, err := client.Get(bad)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("篡改签名竟然返回 %d", resp.StatusCode)
		}
	})

	t.Run("匿名访问私有桶必须被拒", func(t *testing.T) {
		anon := fmt.Sprintf("https://%s.%s/%s", c.Bucket(), "oss-cn-hangzhou.aliyuncs.com", key)
		resp, err := client.Get(anon)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("私有桶居然匿名可读")
		}
	})
}
