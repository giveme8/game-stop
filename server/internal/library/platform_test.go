package library

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"
)

type platformFixture struct {
	db         *gorm.DB
	h          http.Handler
	game       string
	users      []User
	tokens     []Token
	management []string
	play       []string
}

func platformSetup(t *testing.T, db *gorm.DB) *platformFixture {
	t.Helper()
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	f := &platformFixture{db: db, h: s.Handler(), game: "platform-" + fmt.Sprint(time.Now().UnixNano())}
	for i := 0; i < 3; i++ {
		u, err := CreateUser(db, fmt.Sprintf("%s-player-%d", f.game, i))
		if err != nil {
			t.Fatal(err)
		}
		token, raw, err := CreateToken(db, u.Name, 2*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		f.users = append(f.users, u)
		f.tokens = append(f.tokens, token)
		f.management = append(f.management, raw)
	}
	if err := db.Create(&Game{ID: f.game, OwnerID: f.users[0].ID, Visibility: "public", Metadata: `{}`, Filename: "fixture.pck"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, raw := range f.management {
		w := f.request("POST", "/play-sessions", raw, "", "")
		platformStatus(t, w, 201)
		var result struct {
			Token     string
			ExpiresAt time.Time
			GameID    string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.GameID != f.game || !strings.HasPrefix(result.Token, "ps_") || len(result.Token) != 46 || time.Until(result.ExpiresAt) < 59*time.Minute || time.Until(result.ExpiresAt) > time.Hour {
			t.Fatal("invalid play credential response")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(result.Token[3:])
		if err != nil || len(decoded) != 32 {
			t.Fatal("credential entropy encoding")
		}
		var session PlaySession
		if err := db.Where("hash = ?", tokenHash(result.Token)).First(&session).Error; err != nil || session.Hash == result.Token {
			t.Fatalf("credential storage: %v", err)
		}
		f.play = append(f.play, result.Token)
	}
	return f
}
func (f *platformFixture) request(method, suffix, management, play, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/v1/games/"+f.game+suffix, strings.NewReader(body))
	if management != "" {
		r.Header.Set("Authorization", "Bearer "+management)
	}
	if play != "" {
		r.Header.Set("X-Play-Token", play)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func platformStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d want %d: %s", w.Code, want, w.Body.String())
	}
}
func scoreBody(board string, value int64, id string) string {
	return fmt.Sprintf(`{"board":%q,"value":%d,"submissionId":%q}`, board, value, id)
}

func exercisePlatform(t *testing.T, db *gorm.DB) {
	f := platformSetup(t, db)
	platformStatus(t, f.request("POST", "/play-sessions", "", "", ""), 401)
	platformStatus(t, f.request("POST", "/play-sessions", f.play[0], "", ""), 401)
	platformStatus(t, f.request("DELETE", "", "", f.play[0], ""), 401)
	platformStatus(t, f.request("POST", "/scores", f.management[0], "", scoreBody("score", 1, "submission-id-000")), 401)
	platformStatus(t, f.request("POST", "/scores", "", f.management[0], scoreBody("score", 1, "submission-id-000")), 401)
	for _, body := range []string{
		`{"board":"score","value":-1,"submissionId":"submission-id-000"}`,
		`{"board":"score","value":9007199254740992,"submissionId":"submission-id-000"}`,
		`{"board":"score","value":1.2,"submissionId":"submission-id-000"}`,
		`{"board":"score","value":null,"submissionId":"submission-id-000"}`,
		`{"board":"score","submissionId":"submission-id-000"}`,
		`{"board":"time-ms","value":0,"submissionId":"submission-id-000"}`,
		`{"board":"other","value":1,"submissionId":"submission-id-000"}`,
		`{"board":"score","value":1,"submissionId":"tiny"}`,
		`{"board":"score","value":1,"submissionId":"submission-id-000","userId":1}`,
		`{"board":"score","value":1,"submissionId":"submission-id-000"} {}`,
	} {
		platformStatus(t, f.request("POST", "/scores", "", f.play[0], body), 400)
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], strings.Repeat("x", 4097)), 413)
	first := f.request("POST", "/scores", "", f.play[0], scoreBody("score", 100, "submission-id-000"))
	platformStatus(t, first, 200)
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 99, "submission-id-001")), 200)
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 200, "submission-id-002")), 200)
	repeat := f.request("POST", "/scores", "", f.play[0], scoreBody("score", 100, "submission-id-000"))
	platformStatus(t, repeat, 200)
	if first.Body.String() != repeat.Body.String() {
		t.Fatal("idempotent response changed")
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 101, "submission-id-000")), 409)
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("time-ms", 100, "submission-id-000")), 409)
	platformStatus(t, f.request("POST", "/scores", "", f.play[1], scoreBody("score", 200, "submission-id-000")), 200)
	platformStatus(t, f.request("POST", "/scores", "", f.play[2], scoreBody("score", 0, "submission-id-000")), 200)
	for _, v := range []int64{400, 200, 300} {
		platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("time-ms", v, fmt.Sprintf("time-submission-%d", v))), 200)
	}
	var timed PlayerScore
	if err := db.Where("game_id = ? AND user_id = ? AND board = ?", f.game, f.users[0].ID, "time-ms").First(&timed).Error; err != nil || timed.Value != 200 {
		t.Fatal("time board not minimized", err)
	}
	// Force identical timestamps to exercise stable account-ID ordering.
	if err := db.Model(&PlayerScore{}).Where("game_id = ?", f.game).Update("achieved_at", time.Now().UTC().Truncate(time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", "/leaderboard?board=score", "", "", "")
	platformStatus(t, w, 200)
	var board struct{ Entries []LeaderboardEntry }
	if err := json.Unmarshal(w.Body.Bytes(), &board); err != nil {
		t.Fatal(err)
	}
	if len(board.Entries) != 3 || board.Entries[0].Rank != 1 || board.Entries[1].Rank != 1 || board.Entries[2].Rank != 3 || board.Entries[0].PlayerName != f.users[0].Name {
		t.Fatalf("wrong ranking: %+v", board)
	}
	for _, forbidden := range []string{"userId", "token", "ownerId", "hash"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("private field leaked")
		}
	}
	for _, suffix := range []string{"?board=unknown", "?board=score&limit=0", "?board=score&limit=101", "?board=score&limit=x", "?board=score&limit=1&limit=2"} {
		platformStatus(t, f.request("GET", "/leaderboard"+suffix, "", "", ""), 400)
	}
	platformStatus(t, f.request("GET", "/leaderboard?board=score&limit=1", "", "", ""), 200)
	// Scoped credentials cannot be reused for another game.
	original := f.game
	f.game = original + "-other"
	if err := db.Create(&Game{ID: f.game, OwnerID: f.users[0].ID, Visibility: "public", Metadata: `{}`, Filename: "fixture.pck"}).Error; err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 1, "another-game-0000")), 401)
	f.game = original
	// Visibility changes affect existing sessions, while the owner can still play.
	if err := db.Model(&Game{}).Where("id = ?", f.game).Update("visibility", "private").Error; err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("GET", "/leaderboard?board=score", "", f.play[0], ""), 404)
	platformStatus(t, f.request("GET", "/leaderboard?board=score", f.management[0], "", ""), 200)
	platformStatus(t, f.request("POST", "/play-sessions", f.management[1], "", ""), 404)
	platformStatus(t, f.request("POST", "/scores", "", f.play[1], scoreBody("score", 1, "private-game-0000")), 404)
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 1, "private-game-0000")), 200)
	if err := RevokeToken(db, f.tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 1, "private-game-0001")), 401)
	if err := db.Model(&Token{}).Where("id = ?", f.tokens[1].ID).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[1], scoreBody("score", 1, "private-game-0001")), 401)
	if err := db.Model(&PlaySession{}).Where("hash = ?", tokenHash(f.play[2])).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("POST", "/scores", "", f.play[2], scoreBody("score", 1, "private-game-0001")), 401)
	if err := db.Model(&Game{}).Where("id = ?", f.game).Update("deleted", true).Error; err != nil {
		t.Fatal(err)
	}
	platformStatus(t, f.request("GET", "/leaderboard?board=score", "", "", ""), 404)
	platformStatus(t, f.request("POST", "/play-sessions", f.management[2], "", ""), 404)
	platformStatus(t, f.request("POST", "/scores", "", f.play[2], scoreBody("score", 1, "deleted-game-0001")), 404)
}

func TestPlatformAPI(t *testing.T) { exercisePlatform(t, testDB(t)) }

func exerciseConcurrentScores(t *testing.T, db *gorm.DB) {
	f := platformSetup(t, db)
	start := make(chan struct{})
	codes := make(chan int, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes <- f.request("POST", "/scores", "", f.play[0], scoreBody("score", int64(i), fmt.Sprintf("concurrent-id-%03d", i))).Code
		}(i)
	}
	close(start)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("concurrent score status %d", code)
		}
	}
	var best PlayerScore
	if err := db.Where("game_id = ? AND user_id = ? AND board = ?", f.game, f.users[0].ID, "score").First(&best).Error; err != nil || best.Value != 23 {
		t.Fatalf("lost best: %+v %v", best, err)
	}
	// Concurrent identical retries must create exactly one receipt.
	start = make(chan struct{})
	codes = make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- f.request("POST", "/scores", "", f.play[0], scoreBody("score", 9007199254740991, "concurrent-retry-000")).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("concurrent retry status %d", code)
		}
	}
	var count int64
	if err := db.Model(&ScoreSubmission{}).Where("game_id = ? AND submission_hash = ?", f.game, tokenHash("concurrent-retry-000")).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("duplicate receipts", count, err)
	}
}
func TestConcurrentScores(t *testing.T) { exerciseConcurrentScores(t, testDB(t)) }
func TestPlatformMySQLIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not configured; SQLite does not validate MySQL row locking")
	}
	db, err := OpenMySQL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	exercisePlatform(t, db)
	exerciseConcurrentScores(t, db)
}

func TestPlatformCORS(t *testing.T) {
	f := platformSetup(t, testDB(t))
	for _, tc := range []struct {
		path, method, headers string
		status                int
	}{
		{"/scores", "POST", "content-type,x-play-token", 204},
		{"/leaderboard", "GET", "", 204},
		{"/scores", "DELETE", "", 405},
		{"/scores", "POST", "Authorization", 403},
		{"/play-sessions", "POST", "content-type", 405},
	} {
		r := httptest.NewRequest("OPTIONS", "/api/v1/games/"+f.game+tc.path, nil)
		r.Header.Set("Origin", "https://game.example")
		r.Header.Set("Access-Control-Request-Method", tc.method)
		r.Header.Set("Access-Control-Request-Headers", tc.headers)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		platformStatus(t, w, tc.status)
		if tc.status == 204 && w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal("CORS missing")
		}
		if tc.path == "/play-sessions" && w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("management CORS opened")
		}
		if w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("credentialed CORS opened")
		}
	}
}

func TestPlatformDBFailure(t *testing.T) {
	f := platformSetup(t, testDB(t))
	sqlDB, _ := f.db.DB()
	sqlDB.Close()
	platformStatus(t, f.request("GET", "/leaderboard?board=score", "", "", ""), 503)
	platformStatus(t, f.request("POST", "/scores", "", f.play[0], scoreBody("score", 1, "failure-score-000")), 503)
	platformStatus(t, f.request("POST", "/play-sessions", f.management[0], "", ""), 503)
}
