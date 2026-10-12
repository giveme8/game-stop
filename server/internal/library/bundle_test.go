package library

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const testPNG = "\x89PNG\r\n\x1a\n" + "rest-of-a-fake-png"

// 用真实的 pck 头做入口，避免"内容校验"这一层挡住容器本身的用例。
func validEntry() []byte { return fixture(4, "res://main.gd") }

type zipEntry struct {
	name string
	data []byte
	mode os.FileMode
	flag uint16
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Flags: e.flag}
		if e.mode != 0 {
			hdr.SetMode(e.mode)
		}
		f, err := w.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func validMeta(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"id": "demo-game", "title": "演示游戏",
		"engine": map[string]any{"name": "godot", "version": "4.7"},
		"source": map[string]any{"repo": "https://github.com/example/demo", "ref": "v1.0.0"},
		"assets": map[string]any{"cover": "assets/cover.png"},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func validBundle(t *testing.T, mutateMeta func(m map[string]any), extra ...zipEntry) []byte {
	t.Helper()
	entries := []zipEntry{
		{name: "game.json", data: validMeta(t, mutateMeta)},
		{name: "assets/cover.png", data: []byte(testPNG)},
		{name: "content/game.pck", data: validEntry()},
	}
	entries = append(entries, extra...)
	return buildZip(t, entries)
}

func inspect(t *testing.T, raw []byte, wantID string) (*BundlePlan, error) {
	t.Helper()
	return InspectBundle(bytes.NewReader(raw), int64(len(raw)), wantID)
}

func TestBundleAcceptsValidContainer(t *testing.T) {
	raw := validBundle(t, func(m map[string]any) {
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	}, zipEntry{name: "assets/shot-1.png", data: []byte(testPNG)})
	if !LooksLikeBundle(raw) {
		t.Fatal("magic not recognised")
	}
	plan, err := inspect(t, raw, "demo-game")
	if err != nil {
		t.Fatalf("valid bundle rejected: %v", err)
	}
	if plan.Entry != "content/game.pck" || plan.Meta.Title != "演示游戏" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if len(plan.Assets) != 2 || plan.Assets[0].Ext != ".png" {
		t.Fatalf("assets wrong: %+v", plan.Assets)
	}
	if plan.Meta.Source.Repo != "https://github.com/example/demo" {
		t.Fatalf("source lost: %+v", plan.Meta.Source)
	}
	// 入口必须能真的读出来，且仍然是一个合法 pck。
	rc, err := plan.OpenEntry()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if version, err := InspectPCK(bytes.NewReader(data), int64(len(data))); err != nil || version != "4.7" {
		t.Fatalf("entry is not a valid pck: %s %v", version, err)
	}
}

// §12 安全清单：路径穿越与畸形路径。
func TestBundleRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{
		"../evil.png", "assets/../../evil.png", "/abs.png", "a\\b.png",
		"a//b.png", "a/./b.png", "C:/evil.png",
		"assets/cover.png ", "assets/%2e%2e.png", "assets/\x00.png",
	} {
		t.Run(name, func(t *testing.T) {
			raw := validBundle(t, func(m map[string]any) {
				m["assets"] = map[string]any{"cover": name}
			}, zipEntry{name: name, data: []byte(testPNG)})
			if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
				t.Fatalf("unsafe path accepted: %q -> %v", name, err)
			}
		})
	}
}

// §12：可执行内容（与 pck 同一份清单，换一层容器也不放宽）。
func TestBundleRejectsExecutableContent(t *testing.T) {
	for _, name := range []string{
		"content/lib.so", "content/evil.exe", "content/evil.dll", "content/x.wasm",
		"content/x.gdextension", "content/lib.so.1", "content/app.hap", "content/x.dex",
		"content/run.ps1", "content/run.bat",
	} {
		t.Run(name, func(t *testing.T) {
			raw := validBundle(t, nil, zipEntry{name: name, data: []byte("payload")})
			if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
				t.Fatalf("executable entry accepted: %q -> %v", name, err)
			}
		})
	}
}

func TestBundleRejectsSymlinkAndEncrypted(t *testing.T) {
	symlink := validBundle(t, nil, zipEntry{name: "assets/link", data: []byte("content/game.pck"), mode: os.ModeSymlink | 0o777})
	if _, err := inspect(t, symlink, "demo-game"); !errors.Is(err, errBundle) {
		t.Fatalf("symlink accepted: %v", err)
	}
	encrypted := validBundle(t, nil, zipEntry{name: "assets/secret.png", data: []byte(testPNG), flag: 0x1})
	if _, err := inspect(t, encrypted, "demo-game"); !errors.Is(err, errBundle) {
		t.Fatalf("encrypted entry accepted: %v", err)
	}
}

// §12：解压炸弹。10 MB 的零字节压完只有几 KB，压缩率远超上限。
func TestBundleRejectsDecompressionBomb(t *testing.T) {
	raw := validBundle(t, nil, zipEntry{name: "assets/bomb.bin", data: make([]byte, 10<<20)})
	if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
		t.Fatalf("decompression bomb accepted: %v", err)
	}
}

func TestBundleRejectsTooManyEntries(t *testing.T) {
	extra := make([]zipEntry, 0, MaxBundleEntries+1)
	for i := 0; i <= MaxBundleEntries; i++ {
		extra = append(extra, zipEntry{name: fmt.Sprintf("assets/pad-%d.txt", i), data: []byte("x")})
	}
	raw := validBundle(t, nil, extra...)
	if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
		t.Fatalf("entry flooding accepted: %v", err)
	}
}

func TestBundleRejectsBadMetadata(t *testing.T) {
	cases := map[string]func(m map[string]any){
		"id 不一致":         func(m map[string]any) { m["id"] = "other-game" },
		"id 非法":          func(m map[string]any) { m["id"] = "Bad_ID" },
		"缺标题":            func(m map[string]any) { m["title"] = "" },
		"标题过长":           func(m map[string]any) { m["title"] = strings.Repeat("t", 600) },
		"未知引擎":           func(m map[string]any) { m["engine"] = map[string]any{"name": "unity", "version": "4.7"} },
		"引擎版本格式错":        func(m map[string]any) { m["engine"] = map[string]any{"name": "godot", "version": "4"} },
		"accent 非法":      func(m map[string]any) { m["accent"] = "red" },
		"仓库地址带凭据":        func(m map[string]any) { m["source"] = map[string]any{"repo": "https://user:pass@github.com/a/b"} },
		"仓库地址协议不对":       func(m map[string]any) { m["source"] = map[string]any{"repo": "git@github.com:a/b.git"} },
		"commit 不是 40 位": func(m map[string]any) { m["source"] = map[string]any{"commit": "abc"} },
		"截图过多": func(m map[string]any) {
			shots := make([]string, MaxScreenshots+1)
			for i := range shots {
				shots[i] = "assets/cover.png"
			}
			m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": shots}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			raw := validBundle(t, mutate)
			if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
				t.Fatalf("bad metadata accepted: %v", err)
			}
		})
	}
}

func TestBundleRejectsBadEntryAndAssets(t *testing.T) {
	t.Run("缺 game.json", func(t *testing.T) {
		raw := buildZip(t, []zipEntry{
			{name: "assets/cover.png", data: []byte(testPNG)},
			{name: "content/game.pck", data: validEntry()},
		})
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("bundle without meta accepted")
		}
	})
	t.Run("game.json 不是 JSON", func(t *testing.T) {
		raw := buildZip(t, []zipEntry{
			{name: "game.json", data: []byte("not json")},
			{name: "assets/cover.png", data: []byte(testPNG)},
			{name: "content/game.pck", data: validEntry()},
		})
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("non-JSON meta accepted")
		}
	})
	t.Run("entry 指向不存在", func(t *testing.T) {
		raw := validBundle(t, func(m map[string]any) { m["entry"] = "content/missing.pck" })
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("missing entry accepted")
		}
	})
	t.Run("entry 不是 pck", func(t *testing.T) {
		raw := validBundle(t, func(m map[string]any) { m["entry"] = "content/game.zip" },
			zipEntry{name: "content/game.zip", data: []byte("PK\x03\x04")})
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("non-pck entry accepted")
		}
	})
	t.Run("缺封面", func(t *testing.T) {
		raw := validBundle(t, func(m map[string]any) { m["assets"] = map[string]any{} })
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("missing cover accepted")
		}
	})
	t.Run("封面不是图片（魔数不符）", func(t *testing.T) {
		raw := buildZip(t, []zipEntry{
			{name: "game.json", data: validMeta(t, nil)},
			{name: "assets/cover.png", data: []byte("#!/bin/sh\necho hi")},
			{name: "content/game.pck", data: validEntry()},
		})
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("non-image cover accepted")
		}
	})
	t.Run("封面超过 4 MiB", func(t *testing.T) {
		// 前 8 字节是合法 PNG 魔数，其余用不易压缩的数据凑到超限，
		// 这样先撞上的是"素材体积"而不是"压缩率"。
		big := append([]byte(testPNG), bytes.Repeat([]byte{0x5a}, MaxCoverBytes+1)...)
		raw := buildZip(t, []zipEntry{
			{name: "game.json", data: validMeta(t, nil)},
			{name: "assets/cover.png", data: big},
			{name: "content/game.pck", data: validEntry()},
		})
		if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
			t.Fatal("oversized cover accepted")
		}
	})
}

func TestLooksLikeBundleDistinguishesPCK(t *testing.T) {
	if LooksLikeBundle(validEntry()) {
		t.Fatal("pck mistaken for a bundle")
	}
	if !LooksLikeBundle(validBundle(t, nil)) {
		t.Fatal("bundle not recognised")
	}
	if LooksLikeBundle([]byte("GDPC")) || LooksLikeBundle(nil) {
		t.Fatal("garbage recognised as a bundle")
	}
}

// 真实封面有几百 KB。嗅探文件头时若沿用"整条必须小于 N 字节"的读取器，
// 每一张真封面都会被误判成非法——这条就是那次真产物才暴露出来的回归。
func TestBundleAcceptsRealisticallySizedAssets(t *testing.T) {
	// 用不易压缩的内容，避免先被压缩率规则拦下，专测体积路径。
	big := append([]byte(testPNG), make([]byte, 300<<10)...)
	for i := 8; i < len(big); i++ {
		big[i] = byte(i*7 + i>>8)
	}
	raw := validBundle(t, func(m map[string]any) {
		m["assets"] = map[string]any{"cover": "assets/cover.png", "screenshots": []string{"assets/shot-1.png"}}
	},
		zipEntry{name: "assets/cover.png", data: big},
		zipEntry{name: "assets/shot-1.png", data: big[:100<<10]},
	)
	plan, err := inspect(t, raw, "demo-game")
	if err != nil {
		t.Fatalf("bundle with realistic assets rejected: %v", err)
	}
	if len(plan.Assets) != 2 || plan.Assets[0].Ext != ".png" {
		t.Fatalf("assets wrong: %+v", plan.Assets)
	}
	if plan.Assets[0].Max != MaxCoverBytes || plan.Assets[1].Max != MaxScreenshotByte {
		t.Fatalf("per-asset limits wrong: %+v", plan.Assets)
	}
}

// 模式显示名由发布方声明。id 必须与排行榜用的字符集一致（否则这个标签永远匹配不上
// 任何成绩），显示名要短、不为空——它是印在网页按钮上的。
func TestBundleValidatesModeLabels(t *testing.T) {
	good := validBundle(t, func(m map[string]any) {
		m["modes"] = []any{
			map[string]any{"id": "easy", "label": "轻松"},
			map[string]any{"id": "blind", "label": "盲找"},
		}
	})
	plan, err := inspect(t, good, "demo-game")
	if err != nil {
		t.Fatalf("合法 modes 被拒: %v", err)
	}
	// 必须一路带到平台元数据里，否则页面拿不到标签。
	if len(plan.PlatformMetadata().Modes) != 2 || plan.PlatformMetadata().Modes[0].Label != "轻松" {
		t.Fatalf("modes 没有进平台元数据: %+v", plan.PlatformMetadata().Modes)
	}

	mode := func(id, label string) map[string]any { return map[string]any{"id": id, "label": label} }
	tooMany := make([]any, 0, 25)
	for i := 0; i < 25; i++ {
		tooMany = append(tooMany, mode(fmt.Sprintf("m%d", i), "x"))
	}
	for name, modes := range map[string][]any{
		"id 非法": {mode("Easy", "轻松")},
		"id 重复": {mode("easy", "轻松"), mode("easy", "简单")},
		"显示名为空": {mode("easy", "")},
		"显示名过长": {mode("easy", strings.Repeat("长", 13))},
		"条目过多":  tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			raw := validBundle(t, func(m map[string]any) { m["modes"] = modes })
			if _, err := inspect(t, raw, "demo-game"); !errors.Is(err, errBundle) {
				t.Fatalf("非法 modes 被接受: %v", err)
			}
		})
	}
}
