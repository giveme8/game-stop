package library

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHomePublicProjection(t *testing.T) {
	db := testDB(t)
	u, err := CreateUser(db, "homepage-owner")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := CreateToken(db, u.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	malicious := `<script>alert("xss")</script>`
	for _, tc := range []struct {
		id, title, visibility string
		deleted               bool
	}{
		{"find-1-to-49", "寻找数字", "public", false},
		{"track-red-balls", "追踪红球", "public", false},
		{"word-chain", malicious, "public", false},
		{"private-secret-id", "PRIVATE_METADATA_SECRET", "private", false},
		{"deleted-secret-id", "DELETED_METADATA_SECRET", "public", true},
	} {
		m, _ := json.Marshal(Metadata{Title: tc.title, Subtitle: malicious, Description: malicious, Tags: []string{malicious, "益智"}, Engine: Engine{Name: "godot", Version: "4.7"}})
		if err := db.Create(&Game{ID: tc.id, OwnerID: u.ID, Visibility: tc.visibility, Metadata: string(m), Filename: "INTERNAL_STORAGE_SECRET.pck", SHA256: "INTERNAL_DIGEST_SECRET", Size: 213416, Deleted: tc.deleted}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, auth := range []string{"", token} {
		r := httptest.NewRequest("GET", "/?token=QUERY_SECRET", nil)
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("home status %d", w.Code)
		}
		for _, expected := range []string{"寻找数字", "追踪红球", "3 款游戏", "&lt;script&gt;", "Godot 4.7", "208.4 KiB", `href="/api/v1/games/word-chain/package.pck"`, "不能在浏览器中直接开玩", "gamestop download"} {
			if !strings.Contains(body, expected) {
				t.Errorf("missing %q", expected)
			}
		}
		for _, secret := range []string{malicious, "PRIVATE_METADATA_SECRET", "DELETED_METADATA_SECRET", "private-secret-id", "deleted-secret-id", "INTERNAL_STORAGE_SECRET", "INTERNAL_DIGEST_SECRET", "QUERY_SECRET", token, "ownerId", "createdAt"} {
			if strings.Contains(body, secret) {
				t.Errorf("leaked unsafe/private data %q", secret)
			}
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatal("missing security headers")
		}
	}
	if err := db.Model(&Game{}).Where("id = ?", "find-1-to-49").Update("visibility", "private").Error; err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(w.Body.String(), "寻找数字") {
		t.Fatal("unpublished card still visible")
	}
}

func TestHomeRoutingEmptyAndFailure(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		status       int
		text         string
	}{
		{"GET", "/", 200, "目前还没有公开的游戏"},
		{"HEAD", "/", 200, ""},
		{"GET", "/404", 404, ""},
		{"HEAD", "/not-found", 404, ""},
		{"GET", "/games/unknown/", 404, ""},
		{"POST", "/", 405, ""},
	} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if tc.method == "HEAD" && tc.path == "/" && w.Body.Len() != 0 {
			t.Fatal("HEAD emitted body")
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer invalid")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	// 首页是公开的：坏凭据按匿名放行，不再 401。带坏 token 连首页都打不开，
	// 是客户端拿着过期 token 时最先撞上的那堵墙。
	if w.Code != 200 {
		t.Fatalf("首页带无效 token 应仍为 200（按匿名），实际 %d", w.Code)
	}
	sqlDB, _ := db.DB()
	sqlDB.Close()
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(method, "/", nil))
		if w.Code != 503 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("failure status/cache")
		}
		if method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("failure HEAD emitted body")
		}
		if method == "GET" && (!strings.Contains(w.Body.String(), "游戏库暂时无法加载") || strings.Contains(w.Body.String(), "sql:")) {
			t.Fatal("unsafe/missing failure state")
		}
	}
}

func TestPackageSize(t *testing.T) {
	for n, expected := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1048576: "1.0 MiB", 536870912: "512.0 MiB"} {
		if got := packageSize(n); got != expected {
			t.Errorf("size %d = %s", n, got)
		}
	}
}
