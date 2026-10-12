package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestLeaderboardTemplateEscapesAndFormats(t *testing.T) {
	malicious := `<script>alert("xss")</script>`
	for _, board := range []string{"score", "time-ms"} {
		var body bytes.Buffer
		page := leaderboardPageData{ID: "example", Title: malicious, Board: board, Mode: "default", Modes: []string{"default"}, Boards: []string{"score", "time-ms"}, Entries: []LeaderboardEntry{{Rank: 1, PlayerName: malicious, Value: 1234, AchievedAt: time.Date(2026, 9, 19, 12, 30, 0, 0, time.UTC)}}}
		if err := leaderboardTemplate.Execute(&body, page); err != nil {
			t.Fatal(err)
		}
		value, direction := "1234 分", "分数越高越好"
		if board == "time-ms" {
			value, direction = "1.234 秒", "用时越低越好"
		}
		for _, want := range []string{value, direction, "&lt;script&gt;", `href="/leaderboards/example?board=score&amp;mode=default"`, `href="/leaderboards/example?board=time-ms&amp;mode=default"`, `aria-current="page"`, "2026-09-19T12:30:00Z", "返回游戏库", `name="viewport"`} {
			if !strings.Contains(body.String(), want) {
				t.Errorf("%s missing %q", board, want)
			}
		}
		if strings.Contains(body.String(), malicious) || strings.Contains(body.String(), "<script") {
			t.Fatal("unescaped markup or JavaScript")
		}
		// Opt-in local visual review artifact; ordinary test runs write nothing.
		if path := os.Getenv("LEADERBOARD_PREVIEW_HTML"); path != "" && board == "score" {
			preview := leaderboardPageData{ID: "find-1-to-49", Title: "寻找数字", Board: board, Entries: []LeaderboardEntry{
				{Rank: 1, PlayerName: "星际漫游者", Value: 98765, AchievedAt: time.Date(2026, 9, 19, 12, 30, 0, 0, time.UTC)},
				{Rank: 2, PlayerName: "再来一局", Value: 84200, AchievedAt: time.Date(2026, 9, 19, 11, 15, 0, 0, time.UTC)},
				{Rank: 3, PlayerName: "PLAYER 03", Value: 72900, AchievedAt: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)},
			}}
			var rendered bytes.Buffer
			if err := leaderboardTemplate.Execute(&rendered, preview); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, rendered.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for value, want := range map[int64]string{0: "0.000 秒", 1: "0.001 秒", 1000: "1.000 秒", 9223372036854775807: "9223372036854775.807 秒"} {
		if got := leaderboardResult(value, "time-ms"); got != want {
			t.Errorf("%d = %s, want %s", value, got, want)
		}
	}
}

func TestLeaderboardPagePublicOnlyAndFailures(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, visibility string
		deleted        bool
	}{
		{"public-game", "public", false},
		{"private-game", "private", false},
		{"deleted-game", "public", true},
	} {
		metadata, _ := json.Marshal(Metadata{Title: "TITLE_" + tc.id})
		if err := db.Create(&Game{ID: tc.id, OwnerID: 7, Visibility: tc.visibility, Deleted: tc.deleted, Metadata: string(metadata), Filename: "SECRET_STORAGE"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// A failing score query proves inaccessible games are rejected before
	// touching scores, even when the current identity owns the game.
	scoreQueries := 0
	if err := db.Callback().Row().Before("gorm:row").Register("test:leaderboard_failure", func(tx *gorm.DB) {
		if tx.Statement.Table != "games" {
			scoreQueries++
			tx.AddError(errors.New("INTERNAL_DATABASE_SECRET"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"private-game", "deleted-game", "unknown-game", "INVALID"} {
		for _, owner := range []uint64{0, 7} {
			r := httptest.NewRequest("GET", "/leaderboards/"+id+"?board=unknown", nil)
			r.SetPathValue("id", id)
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, owner))
			w := httptest.NewRecorder()
			s.leaderboardPage(w, r)
			if w.Code != 404 || !strings.Contains(w.Body.String(), "未找到公开游戏") || strings.Contains(w.Body.String(), "TITLE_") || scoreQueries != 0 {
				t.Fatalf("id=%s owner=%d status=%d scoreQueries=%d", id, owner, w.Code, scoreQueries)
			}
		}
	}
	for _, tc := range []struct {
		board, method string
		status        int
		text          string
	}{
		{"score", "GET", 503, "排行榜暂时无法加载"},
		{"time-ms", "GET", 503, "排行榜暂时无法加载"},
		{"score", "HEAD", 503, ""},
		{"unsupported", "GET", 400, "不支持的排行榜类型"},
	} {
		r := httptest.NewRequest(tc.method, "/leaderboards/public-game?board="+tc.board, nil)
		r.SetPathValue("id", "public-game")
		w := httptest.NewRecorder()
		s.leaderboardPage(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.text) {
			t.Errorf("%s %s: %d %s", tc.method, tc.board, w.Code, w.Body.String())
		}
		if tc.method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("HEAD emitted body")
		}
		if strings.Contains(w.Body.String(), "INTERNAL_DATABASE_SECRET") || strings.Contains(w.Body.String(), "SECRET_STORAGE") {
			t.Fatal("internal data leaked")
		}
		if w.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("missing security headers")
		}
	}
	if scoreQueries == 0 {
		t.Fatal("public game did not query scores")
	}
	if err := db.Callback().Row().Remove("test:leaderboard_failure"); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.Close()
	r := httptest.NewRequest("GET", "/leaderboards/public-game", nil)
	r.SetPathValue("id", "public-game")
	w := httptest.NewRecorder()
	s.leaderboardPage(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "排行榜暂时无法加载") || strings.Contains(w.Body.String(), "TITLE_") || strings.Contains(w.Body.String(), "sql:") {
		t.Fatal("unsafe database failure state")
	}
}

func TestLeaderboardPageEmptyAndHomeLink(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Game{ID: "empty-game", OwnerID: 1, Visibility: "public", Metadata: `{"title":"空榜游戏"}`, Filename: "private-storage"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, board := range []string{"", "score", "time-ms"} {
		r := httptest.NewRequest("GET", "/leaderboards/empty-game?board="+board, nil)
		r.SetPathValue("id", "empty-game")
		w := httptest.NewRecorder()
		s.leaderboardPage(w, r)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "这个榜单还没有成绩") || !strings.Contains(w.Body.String(), "空榜游戏") {
			t.Fatalf("empty board %q: %d %s", board, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.home(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="/leaderboards/empty-game?board=score"`) {
		t.Fatal("missing homepage leaderboard link")
	}
}

// The in-game button always passes the current difficulty. Before anyone has
// scored in that difficulty the page must still render an empty board: a 400
// here would look like a broken link to the player.
func TestLeaderboardPageAcceptsUnscoredMode(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Game{ID: "mode-game", OwnerID: 1, Visibility: "public", Metadata: `{"title":"分榜游戏"}`, Filename: "storage"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"normal", "blind", "grade1"} {
		r := httptest.NewRequest("GET", "/leaderboards/mode-game?board=time-ms&mode="+mode, nil)
		r.SetPathValue("id", "mode-game")
		w := httptest.NewRecorder()
		s.leaderboardPage(w, r)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.Contains(body, "这个榜单还没有成绩") || !strings.Contains(body, "mode="+mode) {
			t.Fatalf("unscored mode %q: %d %s", mode, w.Code, body)
		}
	}
	// Malformed modes and boards are still rejected.
	for _, query := range []string{"?board=time-ms&mode=Bad_Mode", "?board=other&mode=normal", "?board=time-ms&mode=normal&mode=blind"} {
		r := httptest.NewRequest("GET", "/leaderboards/mode-game"+query, nil)
		r.SetPathValue("id", "mode-game")
		w := httptest.NewRecorder()
		s.leaderboardPage(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d, want 400", query, w.Code)
		}
	}
}

func TestLeaderboardPageVisibilityChangesAfterPrecheck(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Game{ID: "race-game", OwnerID: 7, Visibility: "public", Metadata: `{"title":"PRIVATE_RACE_TITLE"}`, Filename: "storage"}).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Query().After("gorm:after_query").Register("test:hide_after_precheck", func(tx *gorm.DB) {
		if tx.Statement.Table == "games" && !changed && tx.Error == nil {
			changed = true
			if err := db.Model(&Game{}).Where("id = ?", "race-game").Update("visibility", "private").Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove("test:hide_after_precheck") })
	r := httptest.NewRequest("GET", "/leaderboards/race-game", nil)
	r.SetPathValue("id", "race-game")
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, uint64(7)))
	w := httptest.NewRecorder()
	s.leaderboardPage(w, r)
	if !changed || w.Code != 404 || strings.Contains(w.Body.String(), "PRIVATE_RACE_TITLE") {
		t.Fatalf("visibility race leaked owner-only page: changed=%t status=%d", changed, w.Code)
	}
}

func TestLeaderboardPageRealEntries(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	player, err := CreateUser(db, `<img src=x onerror=alert(1)>`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Game{ID: "ranked-game", OwnerID: player.ID, Visibility: "public", Metadata: `{"title":"成绩游戏"}`, Filename: "storage"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, board := range []string{"score", "time-ms"} {
		if err := db.Create(&PlayerScore{GameID: "ranked-game", UserID: player.ID, Board: board, Value: 1234, AchievedAt: time.Now().UTC()}).Error; err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/leaderboards/ranked-game?board="+board, nil))
		value := "1234 分"
		if board == "time-ms" {
			value = "1.234 秒"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), value) || !strings.Contains(w.Body.String(), "&lt;img") || strings.Contains(w.Body.String(), "<img") {
			t.Fatalf("board %s invalid rendered score, status=%d", board, w.Code)
		}
	}
}

// 不带 mode 打开排行榜时，必须落在**有成绩的那个榜**上。
//
// 真机上踩过：游戏提交的是 mode=easy，而页面默认写死 mode=default（没人写的遗留值），
// 于是玩家刚打完一局却看到空榜，以为成绩没上去。
func TestLeaderboardPageDefaultsToAModeWithScores(t *testing.T) {
	db := testDB(t)
	u, err := CreateUser(db, "board-default-player")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Game{
		ID: "find-1-to-49", OwnerID: 1, Visibility: "public",
		Metadata: `{"title":"找数字","engine":{"name":"godot","version":"4.7"}}`,
		Filename: "f.pck",
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 只有 time-ms + easy 有成绩；score 与 default 一条都没有。
	if err := db.Create(&PlayerScore{
		GameID: "find-1-to-49", UserID: u.ID, Board: "time-ms", Mode: "easy",
		Value: 30757, AchievedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	// 一个参数都不给：board 与 mode 都要自己推导到有成绩的那一组。
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/leaderboards/find-1-to-49", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "当前模式</span>") && !strings.Contains(body, "easy") {
		t.Fatalf("页面没有落在 easy 上:\n%s", body[:min(600, len(body))])
	}
	// 成绩本体必须在页面上——这才是用户真正要看到的东西。
	if !strings.Contains(body, "30.757") && !strings.Contains(body, "30757") {
		t.Fatalf("默认视图里看不到那条成绩")
	}
	// 显式指定一个没有成绩的模式，仍然要能打开（空榜，不是 400）。
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, httptest.NewRequest("GET", "/leaderboards/find-1-to-49?board=time-ms&mode=insane", nil))
	if w2.Code != 200 {
		t.Fatalf("显式空模式应 200，实际 %d", w2.Code)
	}
}

// 页面要"人话"：模式显示发布方声明的中文名、时间按北京时间、不列永远空的死模式。
func TestLeaderboardPageSpeaksHuman(t *testing.T) {
	db := testDB(t)
	u, err := CreateUser(db, "board-human-player")
	if err != nil {
		t.Fatal(err)
	}
	meta := `{"title":"找数字","engine":{"name":"godot","version":"4.7"},` +
		`"modes":[{"id":"easy","label":"轻松"},{"id":"normal","label":"普通"},{"id":"zen","label":"不限时"}]}`
	if err := db.Create(&Game{ID: "find-1-to-49", OwnerID: 1, Visibility: "public", Metadata: meta, Filename: "f.pck"}).Error; err != nil {
		t.Fatal(err)
	}
	// 12:13:53 UTC = 20:13 北京时间。
	achieved := time.Date(2026, 9, 20, 12, 13, 53, 0, time.UTC)
	if err := db.Create(&PlayerScore{
		GameID: "find-1-to-49", UserID: u.ID, Board: "time-ms", Mode: "easy",
		Value: 30757, AchievedAt: achieved,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 再给 zen 一条：这样两个难度都会出现在选择器里，才能验顺序。
	if err := db.Create(&PlayerScore{
		GameID: "find-1-to-49", UserID: u.ID, Board: "time-ms", Mode: "zen",
		Value: 42593, AchievedAt: achieved.Add(-time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/leaderboards/find-1-to-49", nil))
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	body := w.Body.String()
	// 1) 难度按钮显示中文名，而不是内部 id。
	if !strings.Contains(body, ">轻松</a>") {
		t.Fatalf("难度按钮没有显示声明的中文名")
	}
	if !strings.Contains(body, "当前模式：轻松") {
		t.Fatalf("当前模式没有用显示名")
	}
	// 内部 id 仍保留在 title 里，便于排障与复制链接（不影响阅读）。
	if !strings.Contains(body, `title="easy"`) {
		t.Fatalf("应保留 id 以便排障")
	}
	// 2) 时间按北京时间，且表头写明。
	if !strings.Contains(body, "2026-09-20 20:13") {
		t.Fatalf("时间没有按北京时间渲染")
	}
	if !strings.Contains(body, "北京时间") || strings.Contains(body, "（UTC）") {
		t.Fatalf("表头时区标注不对")
	}
	// 3) 没有任何成绩的模式不该出现（尤其那个遗留的 default）。
	if strings.Contains(body, ">default</a>") {
		t.Fatalf("永远空的 default 不该作为按钮出现")
	}
	// 声明了但没有成绩的难度不出现在选择器里（普通/困难都没有成绩）。
	if strings.Contains(body, ">普通</a>") || strings.Contains(body, ">困难</a>") {
		t.Fatalf("没有成绩的难度不该出现在选择器里")
	}
	// 4) 一个成绩都没有的游戏，不该出现任何模式按钮（更不该出现 default）。
	if err := db.Model(&Game{}).Where("id = ?", "find-1-to-49").Update("id", "empty-game").Error; err != nil {
		t.Fatal(err)
	}
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, httptest.NewRequest("GET", "/leaderboards/empty-game", nil))
	if w2.Code != 200 {
		t.Fatalf("空游戏页面 status=%d", w2.Code)
	}
	if strings.Contains(w2.Body.String(), ">default</a>") {
		t.Fatalf("没有成绩的游戏不该出现 default 按钮")
	}
	if !strings.Contains(w2.Body.String(), "第一份纪录") {
		t.Fatalf("空游戏应给出空状态提示")
	}
	// 空游戏连"当前模式"都不该提——那里写着 default，用户根本不知道那是什么。
	if strings.Contains(w2.Body.String(), "当前模式") {
		t.Fatalf("没有可选项时不该显示当前模式")
	}
	// 难度按钮要按发布方声明的顺序（游戏里的难度梯度），不是中文标签的字典序。
	// 这里 easy 声明在 zen 之前，而"不限时"按字典序排在"轻松"前面——顺序必须跟声明走。
	if i, j := strings.Index(body, "轻松"), strings.Index(body, "不限时"); i < 0 || j < 0 || i > j {
		t.Fatalf("难度按钮顺序应跟声明走：轻松(%d) 应在 不限时(%d) 之前", i, j)
	}
	// 只提交"用时"的游戏不该出现"分数榜"按钮——它永远是空的（用户直接问过这句）。
	if strings.Contains(body, "分数榜") {
		t.Fatalf("这个游戏只有用时成绩，不该出现分数榜按钮")
	}
	if !strings.Contains(body, "用时榜") {
		t.Fatalf("有成绩的榜必须出现")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
