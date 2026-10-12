package library

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func resetAuthGate() { accountGate = authLimiter{} }

type authFixture struct {
	db *gorm.DB
	h  http.Handler
	ip string
}

func newAuthFixture(t *testing.T, ip string) *authFixture {
	t.Helper()
	resetAuthGate()
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	return &authFixture{db: db, h: s.Handler(), ip: ip}
}

func (f *authFixture) do(method, path, bearer, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = f.ip + ":34567"
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func (f *authFixture) register(name, password string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]string{"name": name, "password": password})
	return f.do("POST", "/api/v1/auth/register", "", string(payload))
}

func (f *authFixture) login(name, password string) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(map[string]string{"name": name, "password": password})
	return f.do("POST", "/api/v1/auth/login", "", string(payload))
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %s", w.Body.String())
	}
	return out
}

func TestPasswordHashing(t *testing.T) {
	encoded, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "correct horse") {
		t.Fatal("plaintext password leaked into the stored value")
	}
	if !verifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if verifyPassword(encoded, "correct horse battery stapl") {
		t.Fatal("wrong password accepted")
	}
	// Salted: the same password must not produce the same stored value.
	second, err := hashPassword("correct horse battery staple")
	if err != nil || second == encoded {
		t.Fatal("password hash is not salted")
	}
	for _, broken := range []string{"", "plain", "pbkdf2-sha256$notanumber$AA$AA", "bcrypt$10$AA$BB",
		"pbkdf2-sha256$100$AA$BB", "pbkdf2-sha256$600000$!!$AA", "pbkdf2-sha256$600000$AAAA$AAAA"} {
		if verifyPassword(broken, "correct horse battery staple") {
			t.Fatalf("malformed hash %q accepted", broken)
		}
	}
}

func TestRegistrationRequiresApproval(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.1")
	if w := f.register("new-player", "a-long-enough-password"); w.Code != 201 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	} else if body := decodeBody(t, w); body["status"] != StatusPending {
		t.Fatalf("registration must start pending: %v", body)
	}
	// The password is stored only as a hash.
	var u User
	if err := f.db.Where("name = ?", "new-player").First(&u).Error; err != nil {
		t.Fatal(err)
	}
	if u.PasswordHash == "" || strings.Contains(u.PasswordHash, "a-long-enough-password") {
		t.Fatal("password stored insecurely")
	}

	// Sign-in works so the player can see the pending state...
	w := f.login("new-player", "a-long-enough-password")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	session := decodeBody(t, w)["sessionToken"].(string)
	if !strings.HasPrefix(session, "ls_") || len(session) != 46 {
		t.Fatalf("unexpected session token %q", session)
	}
	// ...but it cannot issue API tokens before approval.
	if w := f.do("POST", "/api/v1/me/tokens", session, `{}`); w.Code != 403 {
		t.Fatalf("pending account issued a token: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", session, ""); w.Code != 200 {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	} else if body := decodeBody(t, w); body["status"] != StatusPending {
		t.Fatalf("me must report pending: %v", body)
	}

	// Admin approval is what unlocks token issuance.
	if _, err := SetUserStatus(f.db, "new-player", StatusActive); err != nil {
		t.Fatal(err)
	}
	login := f.login("new-player", "a-long-enough-password")
	if login.Code != 200 {
		t.Fatalf("login after approval: %d %s", login.Code, login.Body.String())
	}
	session = decodeBody(t, login)["sessionToken"].(string)
	w = f.do("POST", "/api/v1/me/tokens", session, `{"ttlHours":24}`)
	if w.Code != 201 {
		t.Fatalf("issue token: %d %s", w.Code, w.Body.String())
	}
	token := decodeBody(t, w)["token"].(string)
	if !apiTokenPattern.MatchString(token) {
		t.Fatalf("unexpected token %q", token)
	}
	// The issued token authenticates API calls and only its digest is stored.
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 200 {
		t.Fatalf("token did not authenticate: %d %s", w.Code, w.Body.String())
	}
	var stored Token
	if err := f.db.Where("user_id = ?", u.ID).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Hash == token || strings.Contains(stored.Hash, token) {
		t.Fatal("token stored in plaintext")
	}
}

func TestRegistrationRejectsBadInput(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.2")
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"short password", `{"name":"okname","password":"short"}`, 400},
		{"short name", `{"name":"ab","password":"a-long-enough-password"}`, 400},
		{"unsafe name", `{"name":"bad name!","password":"a-long-enough-password"}`, 400},
		{"unknown field", `{"name":"okname","password":"a-long-enough-password","admin":true}`, 400},
		{"missing password", `{"name":"okname"}`, 400},
		{"not json", `not-json`, 400},
		{"oversized", `{"name":"okname","password":"` + strings.Repeat("x", 5000) + `"}`, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if w := f.do("POST", "/api/v1/auth/register", "", tc.body); w.Code != tc.want {
				t.Fatalf("got %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	if w := f.register("taken-name", "a-long-enough-password"); w.Code != 201 {
		t.Fatalf("first registration: %d %s", w.Code, w.Body.String())
	}
	if w := f.register("taken-name", "another-long-password"); w.Code != 409 {
		t.Fatalf("duplicate name must conflict: %d %s", w.Code, w.Body.String())
	}
}

func TestLoginFailuresDoNotConfirmAccounts(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.3")
	if _, err := CreateUserWithPassword(f.db, "known-player", "a-long-enough-password", StatusActive); err != nil {
		t.Fatal(err)
	}
	unknown := f.login("no-such-player", "a-long-enough-password")
	wrong := f.login("known-player", "wrong-but-long-enough")
	if unknown.Code != 401 || wrong.Code != 401 {
		t.Fatalf("want 401 for both, got %d and %d", unknown.Code, wrong.Code)
	}
	if unknown.Body.String() != wrong.Body.String() {
		t.Fatalf("responses differ and disclose account existence: %q vs %q", unknown.Body.String(), wrong.Body.String())
	}
	// Admin-provisioned accounts without a password cannot be signed in to.
	if _, err := CreateUser(f.db, "token-only"); err != nil {
		t.Fatal(err)
	}
	if w := f.login("token-only", "a-long-enough-password"); w.Code != 401 {
		t.Fatalf("passwordless account signed in: %d", w.Code)
	}
	// Rejected accounts are told apart on purpose so the player can act on it.
	if _, err := CreateUserWithPassword(f.db, "rejected-player", "a-long-enough-password", StatusRejected); err != nil {
		t.Fatal(err)
	}
	if w := f.login("rejected-player", "a-long-enough-password"); w.Code != 403 {
		t.Fatalf("rejected account: %d %s", w.Code, w.Body.String())
	}
}

func TestCredentialsAreNotInterchangeable(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.4")
	if _, err := CreateUserWithPassword(f.db, "split-player", "a-long-enough-password", StatusActive); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Create(&Game{ID: "split-game", OwnerID: 1, Visibility: "public", Metadata: `{}`, Filename: "f.pck"}).Error; err != nil {
		t.Fatal(err)
	}
	login := f.login("split-player", "a-long-enough-password")
	session := decodeBody(t, login)["sessionToken"].(string)
	issued := f.do("POST", "/api/v1/me/tokens", session, `{}`)
	token := decodeBody(t, issued)["token"].(string)

	// A login session must not stand in for an API token.
	if w := f.do("POST", "/api/v1/games/split-game/play-sessions", session, ""); w.Code != 401 {
		t.Fatalf("session acted as an API token: %d %s", w.Code, w.Body.String())
	}
	// An API token must not mint more tokens.
	if w := f.do("POST", "/api/v1/me/tokens", token, `{}`); w.Code != 401 {
		t.Fatalf("API token minted another token: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me/tokens", token, ""); w.Code != 401 {
		t.Fatalf("API token listed tokens: %d", w.Code)
	}
	// ...and an API token can do the API-token things.
	if w := f.do("POST", "/api/v1/games/split-game/play-sessions", token, ""); w.Code != 201 {
		t.Fatalf("API token could not create a play session: %d %s", w.Code, w.Body.String())
	}
	// A malformed credential is refused outright.
	for _, bad := range []string{"gs_short", "ls_" + strings.Repeat("a", 44), strings.Repeat("a", 46)} {
		if w := f.do("GET", "/api/v1/me", bad, ""); w.Code != 401 {
			t.Fatalf("credential %q accepted: %d", bad, w.Code)
		}
	}
}

func TestTokenManagementIsScopedToOwner(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.5")
	for _, name := range []string{"owner-one", "owner-two"} {
		if _, err := CreateUserWithPassword(f.db, name, "a-long-enough-password", StatusActive); err != nil {
			t.Fatal(err)
		}
	}
	sessionOne := decodeBody(t, f.login("owner-one", "a-long-enough-password"))["sessionToken"].(string)
	sessionTwo := decodeBody(t, f.login("owner-two", "a-long-enough-password"))["sessionToken"].(string)

	mine := decodeBody(t, f.do("POST", "/api/v1/me/tokens", sessionOne, `{"ttlHours":1}`))
	id := mine["id"].(string)
	token := mine["token"].(string)

	list := f.do("GET", "/api/v1/me/tokens", sessionTwo, "")
	if list.Code != 200 {
		t.Fatalf("list: %d", list.Code)
	}
	if strings.Contains(list.Body.String(), id) {
		t.Fatal("another account's token id leaked")
	}
	if strings.Contains(list.Body.String(), token) || strings.Contains(list.Body.String(), "hash") {
		t.Fatal("token material leaked in the list")
	}
	// Cross-account revocation must not work.
	if w := f.do("DELETE", "/api/v1/me/tokens/"+id, sessionTwo, ""); w.Code != 404 {
		t.Fatalf("cross-account revoke: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 200 {
		t.Fatal("token was revoked by another account")
	}
	// The owner can revoke, and the token stops working immediately.
	if w := f.do("DELETE", "/api/v1/me/tokens/"+id, sessionOne, ""); w.Code != 204 {
		t.Fatalf("owner revoke: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 401 {
		t.Fatalf("revoked token still works: %d", w.Code)
	}
	if w := f.do("DELETE", "/api/v1/me/tokens/"+id, sessionOne, ""); w.Code != 404 {
		t.Fatalf("double revoke: %d", w.Code)
	}
	// Signing out invalidates the session itself.
	if w := f.do("POST", "/api/v1/auth/logout", sessionOne, ""); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", sessionOne, ""); w.Code != 401 {
		t.Fatal("session survived logout")
	}
}

func TestExpiredSessionIsRejected(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.6")
	if _, err := CreateUserWithPassword(f.db, "sleepy-player", "a-long-enough-password", StatusActive); err != nil {
		t.Fatal(err)
	}
	session := decodeBody(t, f.login("sleepy-player", "a-long-enough-password"))["sessionToken"].(string)
	if err := f.db.Model(&LoginSession{}).Where("hash = ?", tokenHash(session)).
		Update("expires_at", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if w := f.do("GET", "/api/v1/me", session, ""); w.Code != 401 {
		t.Fatalf("expired session accepted: %d", w.Code)
	}
}

func TestAuthRateLimits(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.7")
	status := 0
	for i := 0; i < 8; i++ {
		status = f.register(fmt.Sprintf("flood-%d", i), "a-long-enough-password").Code
	}
	if status != 429 {
		t.Fatalf("registration flooding was not throttled, last status %d", status)
	}
	// A different client is unaffected by the first client's window.
	if _, err := CreateUserWithPassword(f.db, "quiet-player", "a-long-enough-password", StatusActive); err != nil {
		t.Fatal(err)
	}
	other := &authFixture{db: f.db, h: f.h, ip: "10.1.0.8"}
	if w := other.login("quiet-player", "a-long-enough-password"); w.Code != 200 {
		t.Fatalf("unrelated client throttled: %d %s", w.Code, w.Body.String())
	}
}

// The rate limiter must see the real player behind nginx, but must not believe a
// header that anyone on the internet can set.
func TestClientIPTrustsProxyOnlyFromLoopback(t *testing.T) {
	direct := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	direct.RemoteAddr = "203.0.113.9:5000"
	direct.Header.Set("X-Real-IP", "10.9.9.9")
	if got := clientIP(direct); got != "203.0.113.9" {
		t.Fatalf("forged X-Real-IP was trusted: %q", got)
	}
	proxied := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	proxied.RemoteAddr = "127.0.0.1:41234"
	proxied.Header.Set("X-Real-IP", "203.0.113.9")
	if got := clientIP(proxied); got != "203.0.113.9" {
		t.Fatalf("proxy header ignored: %q", got)
	}
	// A loopback peer without the header still works, and nonsense is ignored.
	bare := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	bare.RemoteAddr = "127.0.0.1:41234"
	if got := clientIP(bare); got != "127.0.0.1" {
		t.Fatalf("bare loopback misread: %q", got)
	}
	junk := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
	junk.RemoteAddr = "127.0.0.1:41234"
	junk.Header.Set("X-Real-IP", "not-an-ip")
	if got := clientIP(junk); got != "127.0.0.1" {
		t.Fatalf("malformed header accepted: %q", got)
	}
}

func TestLegacyAdminAccountsStayActive(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.9")
	// Admin CLI provisioning keeps working without a password.
	u, err := CreateUser(f.db, "cli-account")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != StatusActive {
		t.Fatalf("admin-created account is not active: %q", u.Status)
	}
	if _, raw, err := CreateToken(f.db, "cli-account", time.Hour); err != nil {
		t.Fatal(err)
	} else if w := f.do("GET", "/api/v1/me", raw, ""); w.Code != 200 {
		t.Fatalf("CLI token rejected: %d %s", w.Code, w.Body.String())
	}
	// Accounts that predate the status column are backfilled as active.
	if err := f.db.Model(&User{}).Where("id = ?", u.ID).Update("status", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(f.db); err != nil {
		t.Fatal(err)
	}
	var reloaded User
	if err := f.db.Where("id = ?", u.ID).First(&reloaded).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.Status != StatusActive {
		t.Fatalf("legacy account was not backfilled: %q", reloaded.Status)
	}
}

func TestApprovalHelpers(t *testing.T) {
	f := newAuthFixture(t, "10.1.0.10")
	if w := f.register("waits-for-review", "a-long-enough-password"); w.Code != 201 {
		t.Fatalf("register: %d", w.Code)
	}
	pending, err := PendingUsers(f.db)
	if err != nil || len(pending) != 1 || pending[0].Name != "waits-for-review" {
		t.Fatalf("pending list wrong: %v %v", pending, err)
	}
	approved, err := SetUserStatus(f.db, "waits-for-review", StatusActive)
	if err != nil || approved.Status != StatusActive || approved.ApprovedAt == nil {
		t.Fatalf("approve failed: %v %v", approved, err)
	}
	if left, _ := PendingUsers(f.db); len(left) != 0 {
		t.Fatalf("approved account still pending: %v", left)
	}
	rejected, err := SetUserStatus(f.db, "waits-for-review", StatusRejected)
	if err != nil || rejected.Status != StatusRejected || rejected.ApprovedAt != nil {
		t.Fatalf("reject failed: %v %v", rejected, err)
	}
	if _, err := SetUserStatus(f.db, "nobody", StatusActive); err == nil {
		t.Fatal("approving a missing account must fail")
	}
	if _, err := SetUserStatus(f.db, "waits-for-review", "bogus"); err == nil {
		t.Fatal("invalid status accepted")
	}
}

// exerciseAccounts runs the whole self-service flow against whichever database is
// in use. SQLite and MySQL differ on the details this path depends on (unique
// index error translation, case-insensitive name collation), so CI's disposable
// MySQL must run it too, not just a fresh SQLite file.
func exerciseAccounts(t *testing.T, db *gorm.DB) {
	t.Helper()
	resetAuthGate()
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	name := "e2e-player-" + suffix
	f := &authFixture{db: db, h: s.Handler(), ip: "10.20.0.1"}

	if w := f.register(name, "a-long-enough-password"); w.Code != 201 {
		t.Fatalf("register on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
	login := f.login(name, "a-long-enough-password")
	if login.Code != 200 {
		t.Fatalf("login on %s: %d %s", db.Dialector.Name(), login.Code, login.Body.String())
	}
	session, _ := decodeBody(t, login)["sessionToken"].(string)
	if w := f.do("POST", "/api/v1/me/tokens", session, `{}`); w.Code != 403 {
		t.Fatalf("pending account issued a token on %s: %d", db.Dialector.Name(), w.Code)
	}
	if _, err := SetUserStatus(db, name, StatusActive); err != nil {
		t.Fatalf("approve on %s: %v", db.Dialector.Name(), err)
	}
	login = f.login(name, "a-long-enough-password")
	if login.Code != 200 {
		t.Fatalf("login after approval on %s: %d %s", db.Dialector.Name(), login.Code, login.Body.String())
	}
	session, _ = decodeBody(t, login)["sessionToken"].(string)
	issued := f.do("POST", "/api/v1/me/tokens", session, `{"ttlHours":1}`)
	if issued.Code != 201 {
		t.Fatalf("issue token on %s: %d %s", db.Dialector.Name(), issued.Code, issued.Body.String())
	}
	token, _ := decodeBody(t, issued)["token"].(string)
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 200 {
		t.Fatalf("issued token rejected on %s: %d", db.Dialector.Name(), w.Code)
	}
	// A duplicate registration must conflict here too, not create a second row.
	if w := f.register(name, "another-long-password"); w.Code != 409 {
		t.Fatalf("duplicate registration on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
	var count int64
	if err := db.Model(&User{}).Where("name = ?", name).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate rows on %s: %d %v", db.Dialector.Name(), count, err)
	}
	// 注销也要在两套数据库上表现一致：凭据销毁、行留作墓碑、名字继续被占用。
	// （MySQL 与 SQLite 在 UPDATE 的行数语义、唯一索引冲突上并不完全一样。）
	if w := f.do("DELETE", "/api/v1/me", session, `{"password":"a-long-enough-password","confirmName":"`+name+`"}`); w.Code != 204 {
		t.Fatalf("close account on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/v1/me", token, ""); w.Code != 401 {
		t.Fatalf("closed account token still works on %s: %d", db.Dialector.Name(), w.Code)
	}
	if w := f.login(name, "a-long-enough-password"); w.Code != 401 {
		t.Fatalf("closed account signed in on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
	var closed User
	if err := db.Where("name = ?", name).First(&closed).Error; err != nil || closed.Status != StatusDeleted || closed.DeletedAt == nil {
		t.Fatalf("tombstone missing on %s: %+v %v", db.Dialector.Name(), closed, err)
	}
	if err := db.Model(&Token{}).Where("user_id = ?", closed.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("tokens survived the close on %s: %d %v", db.Dialector.Name(), count, err)
	}
	if w := f.register(name, "a-long-enough-password"); w.Code != 409 {
		t.Fatalf("closed name was reusable on %s: %d %s", db.Dialector.Name(), w.Code, w.Body.String())
	}
}

func TestAccountFlowSQLite(t *testing.T) { exerciseAccounts(t, testDB(t)) }

// 无效凭据不能让**公开**端点也挂掉。
//
// 客户端环境里留着过期 token 是常态；旧行为对任何无效 Authorization 一律 401，
// 于是连 /healthz 和公开清单都读不到——恰恰是想排查"token 为什么被拒"的人最需要的
// 那两个接口，而且 CLI 会把所有请求都带上那个坏 token，只读命令也一起废掉。
// 需要凭据的接口仍然要拒绝，并且能区分"没带"和"带了但无效"。
func TestBadCredentialFallsBackToAnonymousOnPublicEndpoints(t *testing.T) {
	db := testDB(t)
	if _, err := CreateUser(db, "public-probe"); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	bad := "Bearer " + "gs_" + strings.Repeat("A", 43)

	public := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/gamesource/v1/index.json"},
	}
	for _, c := range public {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Header.Set("Authorization", bad)
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s 带无效 token 应仍为 200（按匿名），实际 %d: %s", c.method, c.path, w.Code, w.Body.String())
		}
	}

	// 需要凭据的接口：带了无效凭据要说"invalid token"，没带才说"token required"。
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/api/v1/games/some-game", strings.NewReader("x"))
	r.Header.Set("Authorization", bad)
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "invalid token") {
		t.Fatalf("无效凭据写接口应为 401 invalid token，实际 %d: %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("PUT", "/api/v1/games/some-game", strings.NewReader("x"))
	s.Handler().ServeHTTP(w2, r2)
	if w2.Code != 401 || !strings.Contains(w2.Body.String(), "token required") {
		t.Fatalf("无凭据写接口应为 401 token required，实际 %d: %s", w2.Code, w2.Body.String())
	}
}
