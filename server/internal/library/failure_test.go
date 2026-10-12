package library

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

type brokenReader struct{ data []byte }

func (b *brokenReader) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, errors.New("connection lost")
}

func TestStorageFailureCleanup(t *testing.T) {
	db := testDB(t)
	u, _ := CreateUser(db, "failure-owner")
	_, raw, _ := CreateToken(db, u.Name, time.Hour)
	app, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	header := base64.StdEncoding.EncodeToString([]byte(`{"title":"failure","engine":{"name":"godot","version":"4.7"}}`))
	checkEmpty := func() {
		t.Helper()
		files, e := os.ReadDir(app.DataDir)
		if e != nil || len(files) != 0 {
			t.Fatalf("orphan files after failure: %v %v", files, e)
		}
	}
	r := httptest.NewRequest("PUT", "/api/v1/games/broken", &brokenReader{data: fixture(4, "main.gd")})
	r.Header.Set("Authorization", "Bearer "+raw)
	r.Header.Set("X-Game-Metadata", header)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("partial upload status %d", w.Code)
	}
	checkEmpty()
	if err := db.Callback().Create().Before("gorm:create").Register("test_fail_game", func(tx *gorm.DB) {
		if tx.Statement.Table == "games" {
			tx.AddError(errors.New("sensitive SQL diagnostic"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("PUT", "/api/v1/games/db-failure", bytes.NewReader(fixture(4, "main.gd")))
	r.Header.Set("Authorization", "Bearer "+raw)
	r.Header.Set("X-Game-Metadata", header)
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("SQL failure response %d %s", w.Code, w.Body.String())
	}
	checkEmpty()
	var count int64
	db.Model(&Game{}).Count(&count)
	if count != 0 {
		t.Fatal("failed upload published game")
	}
	sqlDB, _ := db.DB()
	sqlDB.Close()
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatal("health ignored database failure")
	}
}

func TestTokenAndConfigValidation(t *testing.T) {
	for _, dsn := range []string{"", "not-a-valid-dsn"} {
		if _, err := OpenMySQL(dsn); err == nil {
			t.Fatal("accepted missing/invalid DSN")
		}
	}
	db := testDB(t)
	CreateUser(db, "alice")
	for _, ttl := range []time.Duration{0, -time.Hour, 366 * 24 * time.Hour} {
		if _, _, err := CreateToken(db, "alice", ttl); err == nil {
			t.Fatalf("accepted ttl %v", ttl)
		}
	}
	if _, err := CreateUser(db, "alice"); err == nil {
		t.Fatal("duplicate account")
	}
	for _, base := range []string{"http://games.example", "https://user:secret@games.example", "https://games.example?query=x", "https://games.example#fragment"} {
		if _, err := NewServer(db, t.TempDir(), base); err == nil {
			t.Fatalf("accepted base %s", base)
		}
	}
}
