package main

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 与 internal/library 的测试夹具同构：一个头部合法、目录可校验的最小 pck。
func testPCK(t *testing.T) []byte {
	t.Helper()
	data := []byte("content")
	name := "res://main.gd"
	dir := new(bytes.Buffer)
	write := func(v any) { _ = binary.Write(dir, binary.LittleEndian, v) }
	write(uint32(1))
	write(uint32(len(name)))
	dir.WriteString(name)
	write(uint64(0))
	write(uint64(len(data)))
	sum := md5.Sum(data)
	dir.Write(sum[:])
	write(uint32(0))
	header := make([]byte, 112)
	copy(header, "GDPC")
	put := binary.LittleEndian.PutUint32
	put(header[4:], 4)
	put(header[8:], 4)
	put(header[12:], 7)
	put(header[20:], 2)
	binary.LittleEndian.PutUint64(header[24:], 112)
	binary.LittleEndian.PutUint64(header[32:], uint64(112+len(data)))
	return append(append(header, data...), dir.Bytes()...)
}

func testBundle(t *testing.T, engineVersion string) []byte {
	t.Helper()
	meta, err := json.Marshal(map[string]any{
		"id": "demo-game", "title": "演示游戏",
		"engine": map[string]any{"name": "godot", "version": engineVersion},
		"source": map[string]any{"repo": "https://github.com/example/demo", "ref": "v1.0.0"},
		"assets": map[string]any{"cover": "assets/cover.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range []struct {
		name string
		data []byte
	}{
		{"game.json", meta},
		{"assets/cover.png", append([]byte("\x89PNG\r\n\x1a\n"), "png"...)},
		{"content/game.pck", testPCK(t)},
	} {
		f, err := w.Create(e.name)
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

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// verify 是发布者唯一的"上传前自检"，误报 OK 比报错更糟，所以正反两面都要锁住。
func TestVerifyPackage(t *testing.T) {
	t.Run("合法 .game", func(t *testing.T) {
		if err := verifyPackage(writeTemp(t, "demo.game", testBundle(t, "4.7")), "demo-game"); err != nil {
			t.Fatalf("valid bundle rejected: %v", err)
		}
	})
	t.Run("合法裸 pck", func(t *testing.T) {
		if err := verifyPackage(writeTemp(t, "demo.pck", testPCK(t)), "demo-game"); err != nil {
			t.Fatalf("valid pck rejected: %v", err)
		}
	})
	t.Run("引擎版本不一致必须报错", func(t *testing.T) {
		err := verifyPackage(writeTemp(t, "mismatch.game", testBundle(t, "4.6")), "demo-game")
		if err == nil {
			t.Fatal("engine mismatch reported OK")
		}
	})
	t.Run("id 不一致必须报错", func(t *testing.T) {
		if err := verifyPackage(writeTemp(t, "id.game", testBundle(t, "4.7")), "other-game"); err == nil {
			t.Fatal("id mismatch reported OK")
		}
	})
	t.Run("既不是容器也不是 pck", func(t *testing.T) {
		if err := verifyPackage(writeTemp(t, "junk.bin", []byte("hello world")), "demo-game"); err == nil {
			t.Fatal("junk reported OK")
		}
	})
	t.Run("空文件", func(t *testing.T) {
		if err := verifyPackage(writeTemp(t, "empty.game", nil), "demo-game"); err == nil {
			t.Fatal("empty file reported OK")
		}
	})
	t.Run("容器里有可执行内容", func(t *testing.T) {
		// 直接把 pck 换成一个 .so 条目：容器校验应当拦下。
		var buf bytes.Buffer
		w := zip.NewWriter(&buf)
		f, _ := w.Create("content/lib.so")
		_, _ = f.Write([]byte("ELF"))
		_ = w.Close()
		if err := verifyPackage(writeTemp(t, "evil.game", buf.Bytes()), "demo-game"); err == nil {
			t.Fatal("unsafe bundle reported OK")
		}
	})
}
