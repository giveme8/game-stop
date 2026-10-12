package library

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPackageBytes int64 = 512 << 20

var errPCK = errors.New("invalid or unsafe PCK")

// InspectPCK implements standalone, unencrypted Godot formats 2/3/4.
// Reference: Godot 4.7-stable core/io/file_access_pack.cpp, try_open_pack.
// V2 has its directory at 96; V3/V4 store a directory offset at 32.
// No embedded executable, sparse bundle, patch/removal, or unknown flags.
func InspectPCK(r io.ReaderAt, size int64) (string, error) {
	if size < 100 || size > MaxPackageBytes {
		return "", errPCK
	}
	h := make([]byte, 40)
	if _, err := r.ReadAt(h, 0); err != nil {
		return "", errPCK
	}
	u32 := binary.LittleEndian.Uint32
	u64 := binary.LittleEndian.Uint64
	if string(h[:4]) != "GDPC" {
		return "", errPCK
	}
	format := u32(h[4:])
	major, minor := u32(h[8:]), u32(h[12:])
	if format < 2 || format > 4 || major != 4 || minor > 999 || u32(h[20:])&^uint32(2) != 0 {
		return "", errPCK
	}
	base := u64(h[24:])
	directory := uint64(96)
	headerEnd := uint64(96)
	if format >= 3 {
		directory = u64(h[32:])
		headerEnd = 104
	}
	if directory < headerEnd || directory > uint64(size)-4 || base > uint64(size) {
		return "", errPCK
	}
	sr := io.NewSectionReader(r, int64(directory), size-int64(directory))
	var count uint32
	if binary.Read(sr, binary.LittleEndian, &count) != nil || count == 0 || count > 100000 {
		return "", errPCK
	}
	type entry struct {
		start, end uint64
		sum        [16]byte
	}
	entries := make([]entry, 0, count)
	seen := map[string]bool{}
	for i := uint32(0); i < count; i++ {
		var n uint32
		if binary.Read(sr, binary.LittleEndian, &n) != nil || n == 0 || n > 4096 {
			return "", errPCK
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(sr, b); err != nil {
			return "", errPCK
		}
		// Godot pads paths with trailing NUL bytes to four-byte alignment.
		name := string(bytes.TrimRight(b, "\x00"))
		if !safePath(name) {
			return "", errPCK
		}
		canonical := strings.ToLower(strings.TrimPrefix(name, "res://"))
		if seen[canonical] {
			return "", errPCK
		}
		seen[canonical] = true
		var off, length uint64
		var sum [16]byte
		var flags uint32
		if binary.Read(sr, binary.LittleEndian, &off) != nil || binary.Read(sr, binary.LittleEndian, &length) != nil {
			return "", errPCK
		}
		if _, err := io.ReadFull(sr, sum[:]); err != nil {
			return "", errPCK
		}
		if binary.Read(sr, binary.LittleEndian, &flags) != nil || flags != 0 {
			return "", errPCK
		}
		if off > uint64(size)-base {
			return "", errPCK
		}
		start := base + off
		if start < headerEnd || length > uint64(size)-start {
			return "", errPCK
		}
		entries = append(entries, entry{start, start + length, sum})
	}
	pos, err := sr.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", errPCK
	}
	directoryEnd := directory + uint64(pos)
	sort.Slice(entries, func(i, j int) bool { return entries[i].start < entries[j].start })
	var previous uint64
	for _, e := range entries {
		if e.start < previous || (e.start < directoryEnd && e.end > directory) {
			return "", errPCK
		}
		previous = e.end
		digest := md5.New()
		if _, err := io.Copy(digest, io.NewSectionReader(r, int64(e.start), int64(e.end-e.start))); err != nil {
			return "", errPCK
		}
		if !bytes.Equal(digest.Sum(nil), e.sum[:]) {
			return "", errPCK
		}
	}
	return fmt.Sprintf("%d.%d", major, minor), nil
}
func safePath(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	s = strings.TrimPrefix(s, "res://")
	if s == "" || strings.ContainsAny(s, "\\:%") || strings.HasPrefix(s, "/") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || p == "." || p == ".." || strings.TrimRight(p, " .") != p {
			return false
		}
	}
	lower := strings.ToLower(s)
	ext := path.Ext(lower)
	switch ext {
	case ".so", ".abc", ".hap", ".har", ".hsp", ".dex", ".apk", ".exe", ".dll", ".dylib", ".com", ".bat", ".cmd", ".ps1", ".msi", ".wasm", ".gdextension":
		return false
	}
	if strings.Contains(path.Base(lower), ".so.") {
		return false
	}
	return true
}
