package library

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	authPageCSP     = "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
	authScriptType  = "text/javascript; charset=utf-8"
	authHTMLType    = "text/html; charset=utf-8"
	authHTMLDocType = "<!doctype html>"
)

func authTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := NewServer(testDB(t), t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func authPageHandler(s *Server, path string) http.HandlerFunc {
	switch path {
	case "/register":
		return s.registerPage
	case "/login":
		return s.loginPage
	case "/account":
		return s.accountPage
	case "/auth.js":
		return s.authScript
	}
	return nil
}

// The three pages and their shared script must be complete, hardened shells:
// correct type, strict headers, no external resource, no template left over.
func TestAuthPagesServeHardenedShells(t *testing.T) {
	s := authTestServer(t)
	cases := []struct {
		path        string
		contentType string
		wants       []string
	}{
		{"/register", authHTMLType, []string{"注册账号", "用户名", "确认密码", "需管理员审核白名单", "register-form", `src="/auth.js"`, `href="/login"`, `name="viewport"`}},
		{"/login", authHTMLType, []string{"登录账号", "用户名", "密码", "login-form", "sessionStorage", `src="/auth.js"`, `href="/register"`, `href="/account"`}},
		{"/account", authHTMLType, []string{"我的账号", "未登录", "明文 Token 只显示一次，请立即保存；不要分享", "token-form", "token-ttl", "token-list", "退出登录", "注销账号", "不可恢复", "close-form", "close-reveal", "close-password", "close-name", "close-submit", `src="/auth.js"`, `href="/login"`}},
		{"/auth.js", authScriptType, []string{"sessionStorage", "未登录", "等待管理员审核", "账号已被拒绝", "账号已注销", "加载失败", "网络超时"}},
	}
	for _, tc := range cases {
		handler := authPageHandler(s, tc.path)
		if handler == nil {
			t.Fatalf("no handler for %s", tc.path)
		}
		for _, method := range []string{"GET", "HEAD"} {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest(method, tc.path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s %s: status %d", method, tc.path, w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != tc.contentType {
				t.Errorf("%s %s: content-type %q", method, tc.path, got)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != authPageCSP {
				t.Errorf("%s %s: csp %q", method, tc.path, got)
			}
			if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("%s %s: referrer-policy %q", method, tc.path, got)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s %s: cache-control %q", method, tc.path, got)
			}
			if method == http.MethodHead {
				if w.Body.Len() != 0 {
					t.Errorf("HEAD %s: emitted a body", tc.path)
				}
				continue
			}
			body := w.Body.String()
			if len(tc.wants) > 0 && !strings.Contains(body, authHTMLDocType) && tc.contentType == authHTMLType {
				t.Errorf("%s: missing doctype", tc.path)
			}
			for _, want := range tc.wants {
				if !strings.Contains(body, want) {
					t.Errorf("%s: missing %q", tc.path, want)
				}
			}
			// No template directive may survive, and no asset may come from
			// anywhere but this origin (there are no external resources at all).
			for _, forbidden := range []string{"{{", "http://", "https://", "<script>", "localStorage", "onerror="} {
				if strings.Contains(body, forbidden) {
					t.Errorf("%s: unexpected %q", tc.path, forbidden)
				}
			}
		}
	}
}

// An unauthenticated /account must answer with the signed-out prompt. It must
// not touch the database, so it keeps working while the database is down.
func TestAccountPageSignedOutWithoutDatabase(t *testing.T) {
	db := testDB(t)
	s, err := NewServer(db, t.TempDir(), "https://games.example")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.accountPage(w, httptest.NewRequest("GET", "/account", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, "未登录：当前浏览器没有登录会话，请先登录。") || !strings.Contains(body, `href="/login"`) {
		t.Fatalf("signed-out page: status=%d body=%s", w.Code, body)
	}
	if !strings.Contains(body, `id="account-view" hidden`) {
		t.Fatal("signed-out page still exposes the Token manager")
	}
	if strings.Contains(body, "账号信息暂时无法加载") || strings.Contains(body, "sql:") {
		t.Fatal("signed-out page leaked a database failure")
	}
}

// Real account data (a username an administrator approved, a Token handle)
// must always be escaped by html/template, never concatenated into markup.
func TestAccountTemplateEscapesAccountData(t *testing.T) {
	malicious := `<script>alert("xss")</script>`
	approved := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	revoked := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	page := accountPageData{
		Name:       malicious,
		StatusText: `<img src=x onerror="alert(1)">`,
		CreatedAt:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ApprovedAt: &approved,
		SignedIn:   true,
		Pending:    true,
		Tokens: []TokenView{
			{ID: `"><script>alert(2)</script>`, CreatedAt: approved, ExpiresAt: revoked},
			{ID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", CreatedAt: approved, ExpiresAt: revoked, RevokedAt: &revoked},
		},
	}
	var body bytes.Buffer
	if err := accountTemplate.Execute(&body, page); err != nil {
		t.Fatal(err)
	}
	out := body.String()
	for _, want := range []string{"&lt;script&gt;", "等待管理员审核", "2026-01-02 03:04:05", "2026-05-06 07:08:09", "2026-06-07 08:09:10", "已撤销", `data-token-id="`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, forbidden := range []string{"<script>alert", `onerror="alert`, `<img`, "alert(2)</script>"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("unescaped %q in rendered account page", forbidden)
		}
	}
	// The only script tag left is the same-origin /auth.js include.
	if strings.Count(out, "<script") != 1 || strings.Count(out, "</script>") != 1 {
		t.Errorf("script tags = %d/%d, want 1/1", strings.Count(out, "<script"), strings.Count(out, "</script>"))
	}
	if !strings.Contains(out, `id="account-view">`) {
		t.Error("signed-in page must reveal the Token manager")
	}
	if !strings.Contains(out, `id="account-signed-out" class="card" hidden`) {
		t.Error("signed-in page must hide the signed-out prompt")
	}
	// A revoked Token offers no revoke button; the active one does.
	if strings.Count(out, ">撤销</button>") != 1 {
		t.Errorf("revoke buttons = %d, want 1", strings.Count(out, ">撤销</button>"))
	}
}

func TestAccountStatusTextDistinguishesStates(t *testing.T) {
	for status, want := range map[string]string{
		StatusActive:   "已通过审核",
		StatusPending:  "等待管理员审核",
		StatusRejected: "账号已被拒绝",
		StatusDeleted:  "账号已注销",
		"":             "账号状态未知",
		"unknown":      "账号状态未知",
	} {
		if got := accountStatusText(status); !strings.Contains(got, want) {
			t.Errorf("status %q = %q, want %q", status, got, want)
		}
	}
}

// The shared script is the only place that knows the API contract and the
// session storage key; it must stay free of any credential or unsafe DOM API.
func TestAuthScriptContractAndCredentialHygiene(t *testing.T) {
	s := authTestServer(t)
	w := httptest.NewRecorder()
	s.authScript(w, httptest.NewRequest("GET", "/auth.js", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`"/api/v1/auth/register"`, `"/api/v1/auth/login"`, `"/api/v1/auth/logout"`, `"/api/v1/me"`, `"/api/v1/me/tokens"`,
		`sessionStorage.setItem(SESSION_KEY, value)`, `sessionStorage.removeItem(SESSION_KEY)`,
		`location.replace("/account")`,
		"AbortController", `credentials: "omit"`, `cache: "no-store"`, `redirect: "error"`, `referrerPolicy: "no-referrer"`, "mode: \"same-origin\"",
		`NAME_PATTERN = /^[a-zA-Z0-9_-]{3,32}$/`, "PASSWORD_MIN = 10", "PASSWORD_MAX = 128", "TOKEN_TTL_MIN = 1", "TOKEN_TTL_MAX = 8760", "TOKEN_TTL_DEFAULT = 720",
		"注册已提交，需管理员审核白名单后才能签发 Token", "凭据错误", "账号已被拒绝", "等待管理员审核", "加载失败", "网络超时", "未登录",
		"明文只显示一次", "不要分享", "复制",
		`method: "DELETE"`, "账号已注销", "确认失败", "密码不正确",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("auth.js missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage.setItem(SESSION_KEY, session", "console.", "innerHTML", "outerHTML", "document.write", "insertAdjacentHTML", "eval(", "Function(",
		"location.href", "location.search", "location.hash", "?token", "gs_", "http://", "https://",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("auth.js contains unsafe or credential-bearing %q", forbidden)
		}
	}
}

// Every element the script looks up must exist in the markup it ships with,
// otherwise a state silently stops rendering.
func TestAuthScriptElementsExistInPages(t *testing.T) {
	lookups := regexp.MustCompile(`el\("([a-z0-9-]+)"\)`).FindAllStringSubmatch(authJS, -1)
	if len(lookups) < 20 {
		t.Fatalf("found only %d element lookups in auth.js", len(lookups))
	}
	declared := map[string]bool{}
	for _, match := range regexp.MustCompile(`id="([a-z0-9-]+)"`).FindAllStringSubmatch(registerHTML+loginHTML+accountHTML, -1) {
		declared[match[1]] = true
	}
	for _, lookup := range lookups {
		if !declared[lookup[1]] {
			t.Errorf("auth.js looks up #%s, which no page declares", lookup[1])
		}
	}
	for _, id := range []string{"register-form", "login-form", "account-view", "token-form", "token-list", "token-secret",
		"close-form", "close-reveal", "close-password", "close-name", "close-submit"} {
		if !declared[id] {
			t.Errorf("page markup lost #%s", id)
		}
	}
}
