package library

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

// closingTestAccount provisions an active account with a login session, an API
// Token, one public game with a score, and one play plus one device session.
func closingTestAccount(t *testing.T, f *authFixture, name string) (User, string, string) {
	t.Helper()
	const password = "a-long-enough-password"
	u, err := CreateUserWithPassword(f.db, name, password, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&Game{ID: name + "-game", OwnerID: u.ID, Visibility: "public",
		Metadata: `{"title":"kept","engine":{"name":"godot","version":"4.7"}}`, Filename: "kept.pck"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&PlayerScore{GameID: name + "-game", UserID: u.ID, Board: "score", Mode: "default",
		Value: 1234, AchievedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	session, _ := decodeBody(t, f.login(name, password))["sessionToken"].(string)
	issued := decodeBody(t, f.do("POST", "/api/v1/me/tokens", session, `{"ttlHours":24}`))
	token, _ := issued["token"].(string)
	id, _ := issued["id"].(string)
	if err := f.db.Create(&PlaySession{Hash: tokenHash("ps_" + strings.Repeat("a", 43)), GameID: name + "-game",
		UserID: u.ID, SourceTokenID: id, ExpiresAt: time.Now().UTC().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&DeviceSession{Hash: tokenHash("gd_" + strings.Repeat("b", 43)), UserCode: "ABCD2345",
		GameID: name + "-game", UserID: u.ID, SourceTokenID: id, ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	return u, session, token
}

func (f *authFixture) closeAccount(bearer, body string) *httptest.ResponseRecorder {
	return f.do("DELETE", "/api/v1/me", bearer, body)
}

func countRows(t *testing.T, db *gorm.DB, model any, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.Model(model).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// 注销必须销毁这个账号的**全部**凭据，同时把不属于凭据的东西（游戏、成绩、
// 别人的凭据）原样留下；账号行本身留作墓碑，名字继续被占用。
func TestAccountDeletionDestroysCredentialsAndKeepsContent(t *testing.T) {
	f := newAuthFixture(t, "10.30.0.1")
	const password = "a-long-enough-password"
	u, session, token := closingTestAccount(t, f, "closing-player")
	other, err := CreateUserWithPassword(f.db, "bystander-player", password, StatusActive)
	if err != nil {
		t.Fatal(err)
	}
	bystanderSession, _ := decodeBody(t, f.login("bystander-player", password))["sessionToken"].(string)

	// 一个 API Token 不是登录会话：它不能注销自己所属的账号。
	if w := f.closeAccount(token, `{"password":"`+password+`","confirmName":"closing-player"}`); w.Code != 401 {
		t.Fatalf("API token closed the account: %d %s", w.Code, w.Body.String())
	}
	// 账号名没输入对：拒绝，且什么都没发生。
	if w := f.closeAccount(session, `{"password":"`+password+`","confirmName":"someone-else"}`); w.Code != 400 {
		t.Fatalf("wrong confirmation name: %d %s", w.Code, w.Body.String())
	}
	// 密码不对：拒绝，账号仍然有效。
	if w := f.closeAccount(session, `{"password":"wrong-but-long-enough","confirmName":"closing-player"}`); w.Code != 403 {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", session, ""); w.Code != 200 {
		t.Fatalf("failed attempts damaged the account: %d %s", w.Code, w.Body.String())
	}

	if w := f.closeAccount(session, `{"password":"`+password+`","confirmName":"closing-player"}`); w.Code != 204 {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}

	var closed User
	if err := f.db.Where("id = ?", u.ID).First(&closed).Error; err != nil {
		t.Fatal(err)
	}
	if closed.Status != StatusDeleted || closed.DeletedAt == nil {
		t.Fatalf("tombstone missing: %+v", closed)
	}
	// 密码不是留空，而是换成一个没人知道的派生值：留空会让登录路径跳过 PBKDF2，
	// 于是"已注销的名字"回答得比"不存在的名字"快，等于确认了账号存在。
	if closed.PasswordHash == "" || closed.PasswordHash == u.PasswordHash || verifyPassword(closed.PasswordHash, password) {
		t.Fatal("the stored password was not replaced by an unusable hash")
	}
	for _, c := range []struct {
		name  string
		model any
	}{
		{"tokens", &Token{}},
		{"login sessions", &LoginSession{}},
		{"play sessions", &PlaySession{}},
		{"device sessions", &DeviceSession{}},
	} {
		if n := countRows(t, f.db, c.model, "user_id = ?", u.ID); n != 0 {
			t.Errorf("%s survived the close: %d", c.name, n)
		}
	}
	// 另一个账号的凭据、以及注销账号的游戏与成绩，都不该被顺手删掉。
	if n := countRows(t, f.db, &LoginSession{}, "user_id = ?", other.ID); n != 1 {
		t.Errorf("bystander login session = %d, want 1", n)
	}
	if w := f.do("GET", "/api/v1/me", bystanderSession, ""); w.Code != 200 {
		t.Fatalf("bystander session broke: %d", w.Code)
	}
	var game Game
	if err := f.db.Where("id = ?", "closing-player-game").First(&game).Error; err != nil {
		t.Fatal(err)
	}
	if game.Deleted || game.Visibility != "public" || game.OwnerID != u.ID {
		t.Fatalf("owned game changed: %+v", game)
	}
	if n := countRows(t, f.db, &PlayerScore{}, "user_id = ?", u.ID); n != 1 {
		t.Errorf("score rows = %d, want 1", n)
	}

	// 凭据全部作废：Token 和登录会话都不再能认证。
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 401 {
		t.Fatalf("closed account token still authenticates: %d", w.Code)
	}
	if w := f.do("GET", "/api/v1/me", session, ""); w.Code != 401 {
		t.Fatalf("closed account session still authenticates: %d", w.Code)
	}
	if w := f.closeAccount(session, `{"password":"`+password+`","confirmName":"closing-player"}`); w.Code != 401 {
		t.Fatalf("closed session could close again: %d %s", w.Code, w.Body.String())
	}
	// 登录失败与"用户名不存在"逐字节相同，注销不会确认账号曾经存在。
	gone := f.login("closing-player", password)
	unknown := f.login("never-registered-player", password)
	if gone.Code != 401 || gone.Body.String() != unknown.Body.String() {
		t.Fatalf("closed account is distinguishable from an unknown name: %d %q vs %d %q",
			gone.Code, gone.Body.String(), unknown.Code, unknown.Body.String())
	}
	// 墓碑占着名字：同一个名字不能再注册。
	if w := f.register("closing-player", password); w.Code != 409 {
		t.Fatalf("closed name was reusable: %d %s", w.Code, w.Body.String())
	}
}

// 两次确认、请求体边界与限流：注销是毁凭据的操作，任何一项不满足都必须原地不动。
func TestAccountDeletionInputGuardsAndRateLimit(t *testing.T) {
	f := newAuthFixture(t, "10.30.0.2")
	const password = "a-long-enough-password"
	u, session, _ := closingTestAccount(t, f, "guarded-player")

	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"missing password", `{"confirmName":"guarded-player"}`, 403},
		{"empty password", `{"password":"","confirmName":"guarded-player"}`, 403},
		{"missing confirm name", `{"password":"` + password + `"}`, 400},
		{"unknown field", `{"password":"` + password + `","confirmName":"guarded-player","admin":true}`, 400},
		{"not json", `not-json`, 400},
		{"oversized", `{"password":"` + strings.Repeat("x", 5000) + `","confirmName":"guarded-player"}`, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := f.closeAccount(session, tc.body); w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	// 大小写不敏感的名字确认是允许的（MySQL 的账号名排序规则本来就不区分大小写）。
	if w := f.closeAccount(session, `{"password":"wrong-but-long-enough","confirmName":"Guarded-Player"}`); w.Code != 403 {
		t.Fatalf("case-insensitive confirmation: %d %s", w.Code, w.Body.String())
	}
	var alive User
	if err := f.db.Where("id = ?", u.ID).First(&alive).Error; err != nil {
		t.Fatal(err)
	}
	if alive.Status != StatusActive || alive.DeletedAt != nil || alive.PasswordHash != u.PasswordHash {
		t.Fatalf("rejected attempts changed the account: %+v", alive)
	}

	// 用错的密码连打，必须在第 6 次之前被限流挡住——否则这个端点就是一条
	// 拿着会话就能暴力猜密码的通道。
	throttled := false
	for i := 0; i < 8; i++ {
		if w := f.closeAccount(session, `{"password":"wrong-but-long-enough","confirmName":"guarded-player"}`); w.Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("repeated wrong passwords were never throttled")
	}
	// 限流期间即使密码正确也不能注销，账号必须完好；换一个窗口后仍可正常注销。
	if w := f.closeAccount(session, `{"password":"`+password+`","confirmName":"guarded-player"}`); w.Code != 429 {
		t.Fatalf("throttled client closed the account: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", session, ""); w.Code != 200 {
		t.Fatalf("throttling damaged the account: %d", w.Code)
	}
	resetAuthGate()
	if w := f.closeAccount(session, `{"password":"`+password+`","confirmName":"guarded-player"}`); w.Code != 204 {
		t.Fatalf("close after the window: %d %s", w.Code, w.Body.String())
	}
}
