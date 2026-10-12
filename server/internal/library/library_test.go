package library

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func fixture(format uint32, name string) []byte {
	data := []byte("content")
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
	put(header[4:], format)
	put(header[8:], 4)
	put(header[12:], 7)
	put(header[20:], 2)
	if format == 2 {
		header = header[:96]
		binary.LittleEndian.PutUint64(header[24:], uint64(96+dir.Len()))
		return append(append(header, dir.Bytes()...), data...)
	}
	binary.LittleEndian.PutUint64(header[24:], 112)
	binary.LittleEndian.PutUint64(header[32:], uint64(112+len(data)))
	return append(append(header, data...), dir.Bytes()...)
}
func TestPCK(t *testing.T) {
	for _, v := range []uint32{2, 3, 4} {
		b := fixture(v, "res://main.gd")
		if got, err := InspectPCK(bytes.NewReader(b), int64(len(b))); err != nil || got != "4.7" {
			t.Fatalf("format %d: %s %v", v, got, err)
		}
	}
	for _, name := range []string{"../bad", "res://a/../../evil", "a\\b", "C:/bad", "a//b", "a/./b", "evil.DLL", "lib.so.1", "a. ", "a\x00b", "a%2fb", "foo.gdextension"} {
		b := fixture(4, name)
		if _, err := InspectPCK(bytes.NewReader(b), int64(len(b))); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	valid := fixture(4, "main.gd")
	for _, test := range []struct {
		name   string
		mutate func([]byte)
	}{
		{"unknown version", func(b []byte) { b[4] = 5 }}, {"directory encryption", func(b []byte) { b[20] = 1 }}, {"sparse", func(b []byte) { b[20] = 4 }},
		{"directory out of range", func(b []byte) { binary.LittleEndian.PutUint64(b[32:], ^uint64(0)) }}, {"base overflow", func(b []byte) { binary.LittleEndian.PutUint64(b[24:], ^uint64(0)) }},
		{"bad digest", func(b []byte) { b[112] ^= 1 }}, {"encrypted entry", func(b []byte) { b[len(b)-4] = 1 }}, {"embedded exe", func(b []byte) { copy(b, "MZ!!") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := bytes.Clone(valid)
			test.mutate(b)
			if _, err := InspectPCK(bytes.NewReader(b), int64(len(b))); err == nil {
				t.Fatal("accepted corrupt PCK")
			}
		})
	}
	for n := 0; n < len(valid); n++ {
		if _, err := InspectPCK(bytes.NewReader(valid[:n]), int64(n)); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
}
func TestRealPCK(t *testing.T) {
	p := "../../../dist/games/hellogodot.pck"
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		t.Skip("local real Godot fixture not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	if ver, err := InspectPCK(f, st.Size()); err != nil || ver != "4.7" {
		t.Fatalf("real exported PCK: version=%s err=%v", ver, err)
	}
}
func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}
func exerciseAPI(t *testing.T, db *gorm.DB) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	a, err := CreateUser(db, "alice"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateUser(db, "bob"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	ta, rawA, err := CreateToken(db, a.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, rawB, err := CreateToken(db, b.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ta.Hash == rawA || len(ta.Hash) != 64 {
		t.Fatal("token not hashed")
	}
	app, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	h := app.Handler()
	id := "game-" + suffix
	endpoint := "/api/v1/games/" + id
	pack := fixture(4, "main.gd")
	request := func(method, path, token string, body []byte, meta string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if meta != "" {
			r.Header.Set("X-Game-Metadata", base64.StdEncoding.EncodeToString([]byte(meta)))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	expect := func(w *httptest.ResponseRecorder, status int) {
		t.Helper()
		if w.Code != status {
			t.Fatalf("status=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
	}
	meta := `{"title":"中文游戏","engine":{"name":"godot","version":"4.7"}}`
	expect(request("PUT", endpoint, "", pack, meta), 401)
	expect(request("PUT", endpoint, rawA, pack, meta), 201)
	expect(request("GET", endpoint, "", nil, ""), 404)
	expect(request("GET", endpoint, rawB, nil, ""), 404)
	expect(request("GET", endpoint, rawA, nil, ""), 200)
	// 坏凭据按匿名处理，所以这里看到的是"私有游戏对外不可见"（404），而不是 401。
	// 意图没变：坏凭据拿不到别人的私有游戏。
	expect(request("GET", endpoint, "bad", nil, ""), 404)
	expect(request("PUT", endpoint, rawB, pack, meta), 409)
	expect(request("PATCH", endpoint, rawB, []byte(`{"visibility":"public"}`), ""), 409)
	expect(request("PATCH", endpoint, rawA, []byte(`{"visibility":"public"}`), ""), 200)
	expect(request("GET", endpoint, "", nil, ""), 200)
	// 此刻游戏已公开：坏凭据按匿名照样能看（公开内容对谁都可见），
	// 区别只在"能不能写"和"能不能看私有游戏"。
	expect(request("GET", endpoint, "bad", nil, ""), 200)
	index := request("GET", "/gamesource/v1/index.json", "", nil, "")
	expect(index, 200)
	etag := index.Header().Get("ETag")
	conditional := httptest.NewRequest("GET", "/gamesource/v1/index.json", nil)
	conditional.Header.Set("If-None-Match", etag)
	cw := httptest.NewRecorder()
	h.ServeHTTP(cw, conditional)
	expect(cw, 304)
	if !strings.Contains(index.Body.String(), id) {
		t.Fatal("public game missing")
	}
	// 公开清单在坏凭据下必须仍可读（这正是踩过的坑：客户端环境里留着过期 token 时，
	// 连诊断用的公开接口都变成 401）。
	expect(request("GET", "/gamesource/v1/index.json", "bad", nil, ""), 200)
	head := request("HEAD", endpoint+"/package.pck", "", nil, "")
	expect(head, 200)
	if head.Header().Get("Content-Length") != fmt.Sprint(len(pack)) || head.Header().Get("X-Checksum-SHA256") == "" {
		t.Fatal("download headers missing")
	}
	rr := httptest.NewRequest("GET", endpoint+"/package.pck", nil)
	rr.Header.Set("Range", "bytes=0-3")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, rr)
	expect(rw, 206)
	if rw.Body.String() != "GDPC" {
		t.Fatal("bad range")
	}
	var first Game
	db.Where("id = ?", id).First(&first)
	expect(request("PUT", endpoint, rawA, pack, meta), 200)
	if _, err := os.Stat(filepath.Join(app.DataDir, first.Filename)); err != nil {
		t.Fatal("historical download removed")
	}
	before, _ := os.ReadDir(app.DataDir)
	incomplete := httptest.NewRequest("PUT", endpoint, bytes.NewReader(pack))
	incomplete.ContentLength = int64(len(pack) + 1)
	incomplete.Header.Set("Authorization", "Bearer "+rawA)
	incomplete.Header.Set("X-Game-Metadata", base64.StdEncoding.EncodeToString([]byte(meta)))
	iw := httptest.NewRecorder()
	h.ServeHTTP(iw, incomplete)
	expect(iw, 400)
	oversized := httptest.NewRequest("PUT", endpoint, bytes.NewReader(pack))
	oversized.ContentLength = MaxPackageBytes + 1
	oversized.Header = incomplete.Header.Clone()
	ow := httptest.NewRecorder()
	h.ServeHTTP(ow, oversized)
	expect(ow, 413)
	expect(request("PUT", endpoint, rawA, []byte("GDPCbad"), meta), 400)
	expect(request("PUT", endpoint, rawA, fixture(4, "evil.exe"), meta), 400)
	after, _ := os.ReadDir(app.DataDir)
	if len(before) != len(after) {
		t.Fatal("failed upload leaked files")
	}
	expect(request("PUT", endpoint, rawA, pack, `{"title":"x","engine":{"name":"godot","version":"4.6"}}`), 400)
	expect(request("PATCH", endpoint, rawA, []byte(`{"visibility":null}`), ""), 400)
	expect(request("PATCH", endpoint, rawA, []byte(`{"ownerId":2}`), ""), 400)
	expect(request("PATCH", endpoint, rawA, []byte(`{"visibility":"private"}`), ""), 200)
	expect(request("GET", endpoint+"/package.pck", "", nil, ""), 404)
	index = request("GET", "/gamesource/v1/index.json", rawA, nil, "")
	if strings.Contains(index.Body.String(), id) {
		t.Fatal("owner private game leaked to public index")
	}
	conditional.Header.Set("If-None-Match", etag)
	cw = httptest.NewRecorder()
	h.ServeHTTP(cw, conditional)
	expect(cw, 200)
	own := request("GET", "/api/v1/games", rawA, nil, "")
	if !strings.Contains(own.Body.String(), id) {
		t.Fatal("owned missing")
	}
	if !strings.Contains(own.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("private caching")
	}
	expect(request("DELETE", endpoint, rawB, nil, ""), 409)
	expect(request("DELETE", endpoint, rawA, nil, ""), 204)
	expect(request("PUT", endpoint, rawB, pack, meta), 409)
	expect(request("GET", endpoint, rawA, nil, ""), 404)
	if err := RevokeToken(db, ta.ID); err != nil {
		t.Fatal(err)
	}
	// 已撤销的凭据不再 401，而是按匿名看到公开列表——但绝不能看到私有游戏。
	// 下面那条断言才是真正要守住的东西。
	revokedList := request("GET", "/api/v1/games", rawA, nil, "")
	expect(revokedList, 200)
	if strings.Contains(revokedList.Body.String(), id) {
		t.Fatal("已撤销的凭据看到了私有游戏")
	}
	expired, rawExpired, _ := CreateToken(db, a.Name, time.Hour)
	db.Model(&Token{}).Where("id = ?", expired.ID).Update("expires_at", time.Now().UTC().Add(-time.Hour))
	expiredList := request("GET", "/api/v1/games", rawExpired, nil, "")
	expect(expiredList, 200)
	if strings.Contains(expiredList.Body.String(), id) {
		t.Fatal("过期的凭据看到了私有游戏")
	}
	expect(request("GET", "/healthz", "", nil, ""), 200)
}
func TestAPI(t *testing.T) { exerciseAPI(t, testDB(t)) }
func TestMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not configured; CI must use disposable MySQL")
	}
	db, err := OpenMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err = Migrate(db); err != nil {
		t.Fatal(err)
	}
	exerciseAPI(t, db)
	testConcurrentOwners(t, db)
	exerciseAccounts(t, db)
	exerciseBundleUpload(t, db)
}
func testConcurrentOwners(t *testing.T, db *gorm.DB) {
	t.Helper()
	suffix := fmt.Sprint(time.Now().UnixNano())
	var tokens []string
	for _, n := range []string{"race-a", "race-b"} {
		u, e := CreateUser(db, n+suffix)
		if e != nil {
			t.Fatal(e)
		}
		_, raw, e := CreateToken(db, u.Name, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		tokens = append(tokens, raw)
	}
	app, e := NewServer(db, t.TempDir(), "https://games.example")
	if e != nil {
		t.Fatal(e)
	}
	h := app.Handler()
	id := "race-" + suffix
	start := make(chan struct{})
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(http.MethodPut, "/api/v1/games/"+id, bytes.NewReader(fixture(4, "main.gd")))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Game-Metadata", base64.StdEncoding.EncodeToString([]byte(`{"title":"race","engine":{"name":"godot","version":"4.7"}}`)))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			results <- w.Code
		}(token)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for c := range results {
		if c == 201 {
			success++
		} else if c != 409 && c != 503 {
			t.Fatalf("race status %d", c)
		}
	}
	if success != 1 {
		t.Fatalf("race successes %d", success)
	}
	var games []Game
	db.Where("id = ?", id).Find(&games)
	if len(games) != 1 {
		t.Fatal("race created duplicate")
	}
	files, _ := os.ReadDir(app.DataDir)
	if len(files) != 1 {
		t.Fatalf("race storage leak %d", len(files))
	}
}
func TestConcurrentOwners(t *testing.T) { testConcurrentOwners(t, testDB(t)) }
func TestMetadataLimits(t *testing.T) {
	m := Metadata{Title: "x", Engine: Engine{"godot", "4.7"}}
	if m.Validate() != nil {
		t.Fatal("valid metadata")
	}
	m.Title = strings.Repeat("x", 513)
	if m.Validate() == nil {
		t.Fatal("title limit")
	}
	b, _ := json.Marshal(m)
	if decodeJSON(append(b, []byte(" {}")...), &m) == nil {
		t.Fatal("trailing JSON")
	}
}
func FuzzInspectPCK(f *testing.F) {
	f.Add(fixture(4, "main.gd"))
	f.Add([]byte("GDPC"))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = InspectPCK(bytes.NewReader(b), int64(len(b))) })
}
