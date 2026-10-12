package library

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserSDKAsset(t *testing.T) {
	s, err := NewServer(testDB(t), t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRecorder()
		s.Handler().ServeHTTP(r, httptest.NewRequest(method, "/sdk/v1/game-stop.js", nil))
		if r.Code != 200 || !strings.Contains(r.Header().Get("Content-Type"), "javascript") || r.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("asset response %d %v", r.Code, r.Header())
		}
		if method == http.MethodHead && r.Body.Len() != 0 {
			t.Fatal("HEAD returned body")
		}
		if method == http.MethodGet && !strings.Contains(r.Body.String(), "class GameStopSDK") {
			t.Fatal("SDK missing")
		}
	}
}
