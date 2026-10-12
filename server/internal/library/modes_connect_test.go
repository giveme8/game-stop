package library

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestScoreModes(t *testing.T) {
	f := platformSetup(t, testDB(t))
	for i, mode := range []string{"default", "easy", "hard"} {
		body := fmt.Sprintf(`{"board":"score","mode":%q,"value":%d,"submissionId":"mode-submission-%02d"}`, mode, i+1, i)
		platformStatus(t, f.request("POST", "/scores", "", f.play[0], body), 200)
		platformStatus(t, f.request("POST", "/scores", "", f.play[0], body), 200)
		w := f.request("GET", "/leaderboard?mode="+mode, "", "", "")
		platformStatus(t, w, 200)
		var result struct {
			Mode    string
			Entries []LeaderboardEntry
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Mode != mode || len(result.Entries) != 1 || result.Entries[0].Value != int64(i+1) {
			t.Fatal("mixed mode leaderboard")
		}
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], `{"board":"score","mode":"hard","value":1,"submissionId":"mode-submission-00"}`), 409)
	for _, mode := range []string{"UPPER", "-x", "a/b", strings.Repeat("a", 65)} {
		platformStatus(t, f.request("POST", "/scores", "", f.play[0], fmt.Sprintf(`{"board":"score","mode":%q,"value":1,"submissionId":"invalid-mode-0000"}`, mode)), 400)
		platformStatus(t, f.request("GET", "/leaderboard?mode="+url.QueryEscape(mode), "", "", ""), 400)
	}
	platformStatus(t, f.request("GET", "/leaderboard?mode=easy&mode=hard", "", "", ""), 400)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", "/leaderboards/"+f.game+"?mode=easy", nil))
	platformStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "mode=hard") || !strings.Contains(w.Body.String(), "当前模式：easy") {
		t.Fatal("missing mode links")
	}
}

func TestOldScoreModeMigration(t *testing.T) {
	db := testDB(t)
	for _, sql := range []string{
		"DROP TABLE player_scores", "DROP TABLE score_submissions",
		"CREATE TABLE player_scores (game_id varchar(64), user_id integer, board varchar(16), value integer NOT NULL, achieved_at datetime NOT NULL, PRIMARY KEY(game_id,user_id,board))",
		"CREATE TABLE score_submissions (game_id varchar(64), user_id integer, submission_hash varchar(64), board varchar(16), value integer, best_value integer, best_achieved_at datetime, created_at datetime, PRIMARY KEY(game_id,user_id,submission_hash))",
		"INSERT INTO player_scores VALUES ('legacy',1,'score',42,'2026-01-01 00:00:00')",
		"INSERT INTO score_submissions VALUES ('legacy',1,'old','score',42,42,'2026-01-01 00:00:00','2026-01-01 00:00:00')",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var best PlayerScore
	if err := db.First(&best).Error; err != nil || best.Mode != "default" || best.Value != 42 {
		t.Fatal("legacy score lost")
	}
	var submission ScoreSubmission
	if err := db.First(&submission).Error; err != nil || submission.Mode != "default" {
		t.Fatal("legacy submission lost")
	}
	best.Mode = "hard"
	if err := db.Create(&best).Error; err != nil {
		t.Fatal("old primary key retained:", err)
	}
}

func TestConnectOrigins(t *testing.T) {
	for _, origin := range []string{"https://games.example", "https://games.example:8443", "http://localhost:8080", "http://127.0.0.1:8080", "http://10.1.2.3", "http://172.16.0.1", "http://192.168.1.10:8080"} {
		if !validConnectOrigin(origin) {
			t.Errorf("rejected %s", origin)
		}
	}
	for _, origin := range []string{"http://games.example", "http://8.8.8.8", "http://172.32.0.1", "http://169.254.1.1", "javascript:alert(1)", "data:text/html,x", "https://x@evil.test", "https://evil.test/", "https://evil.test?", "https://evil.test#", "https://evil.test/path", "https://evil.test:443", "https://evil.test:0", "https://evil.test:", "https://127.1", "http://2130706433", "https://evil.test\\x", "https://EXAMPLE.COM", "null"} {
		if validConnectOrigin(origin) {
			t.Errorf("accepted %s", origin)
		}
	}
}

func TestConnectPageSecurity(t *testing.T) {
	f := platformSetup(t, testDB(t))
	path := "/connect?gameId=" + f.game + "&mode=easy&origin=" + url.QueryEscape("http://192.168.1.10:8080") + "&state=" + strings.Repeat("a", 32)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	platformStatus(t, w, 200)
	for key, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "Cross-Origin-Opener-Policy": "unsafe-none", "X-Frame-Options": "DENY"} {
		if w.Header().Get(key) != want {
			t.Errorf("header %s", key)
		}
	}
	for _, want := range []string{"script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), want) {
			t.Fatal("CSP missing", want)
		}
	}
	if !strings.Contains(w.Body.String(), "局域网 HTTP 信任边界") || !strings.Contains(w.Body.String(), "type=\"password\"") || strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("unsafe page")
	}
	for _, suffix := range []string{"&gameId=other", "&token=forbidden", "&mode=hard"} {
		w = httptest.NewRecorder()
		f.h.ServeHTTP(w, httptest.NewRequest("GET", path+suffix, nil))
		platformStatus(t, w, 400)
	}
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", "/connect?gameId=test", nil))
	platformStatus(t, w, 400)
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("GET", "/connect.js", nil))
	platformStatus(t, w, 200)
	if strings.Contains(w.Body.String(), "localStorage") || strings.Contains(w.Body.String(), "console.") {
		t.Fatal("credential persistence/logging")
	}
}
