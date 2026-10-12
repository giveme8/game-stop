package library

import (
	"bytes"
	_ "embed"
	"errors"
	"html/template"
	"net/http"
	"time"
)

// Registration, sign-in and account pages.
//
// The pages are deliberately thin shells: every state comes from the JSON API
// through the shared /auth.js script, so no credential ever travels through a
// URL, a cookie, a log line or a rendered attribute. The account page also
// pre-renders the identity when the request itself already carries a valid
// login-session bearer, which keeps a readable (no-JavaScript) fallback and
// lets tests exercise template escaping with real account data.

//go:embed register.html
var registerHTML string

//go:embed login.html
var loginHTML string

//go:embed account.html
var accountHTML string

//go:embed auth.js
var authJS string

var registerTemplate = template.Must(template.New("register").Parse(registerHTML))
var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

var accountTemplate = template.Must(template.New("account").Funcs(template.FuncMap{
	"date":    authTimestamp,
	"dateptr": authTimestampPtr,
}).Parse(accountHTML))

func authTimestamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

func authTimestampPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return authTimestamp(*t)
}

// accountPageData carries account metadata and Token handles only: a Token's
// plaintext exists solely in the API response of the issuing request, so it can
// never be re-rendered here even by accident.
type accountPageData struct {
	Name        string
	StatusText  string
	CreatedAt   time.Time
	ApprovedAt  *time.Time
	Tokens      []TokenView
	SignedIn    bool
	Pending     bool
	Rejected    bool
	CanIssue    bool
	Unavailable bool
}

func accountStatusText(status string) string {
	switch status {
	case StatusActive:
		return "已通过审核：可以签发 Token。"
	case StatusPending:
		return "等待管理员审核：审核通过前不能签发 Token。"
	case StatusRejected:
		return "账号已被拒绝：不能签发 Token，请联系管理员。"
	case StatusDeleted:
		return "账号已注销：凭据已全部销毁，无法再登录或签发 Token。"
	default:
		return "账号状态未知：请联系管理员确认。"
	}
}

// authPageHeaders is the shared policy of all three auth pages: nothing may be
// cached, embedded by another origin, or loaded from anywhere but this origin.
func authPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeHTMLPage(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data any) {
	var body bytes.Buffer
	if err := tmpl.Execute(&body, data); err != nil {
		fail(w, http.StatusServiceUnavailable, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}
}

func (s *Server) registerPage(w http.ResponseWriter, r *http.Request) {
	authPageHeaders(w)
	writeHTMLPage(w, r, registerTemplate, struct{}{})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	authPageHeaders(w)
	writeHTMLPage(w, r, loginTemplate, struct{}{})
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request) {
	authPageHeaders(w)
	page := accountPageData{}
	if id := sessionUserID(r); id != 0 {
		summary, err := s.accountSummary(r, id)
		switch {
		case err == nil:
			page.SignedIn = true
			page.Name = summary.Name
			page.StatusText = accountStatusText(summary.Status)
			page.CreatedAt = summary.CreatedAt
			page.ApprovedAt = summary.ApprovedAt
			page.Pending = summary.Status == StatusPending
			page.Rejected = summary.Status == StatusRejected
			page.CanIssue = summary.Status == StatusActive
			if tokens, err := s.tokensFor(id); err == nil {
				page.Tokens = tokens
			}
		case errors.Is(err, errMissing):
			// The session outlived its account: show the signed-out prompt
			// instead of a failure, and let the script clear the session.
		default:
			page.Unavailable = true
		}
	}
	writeHTMLPage(w, r, accountTemplate, page)
}

func (s *Server) authScript(w http.ResponseWriter, r *http.Request) {
	authPageHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(authJS))
	}
}
