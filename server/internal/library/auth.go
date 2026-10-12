package library

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Passwords are stored as PBKDF2-HMAC-SHA256. The standard library has had
// crypto/pbkdf2 since Go 1.24, so this needs no module dependency; bcrypt or
// argon2 would be preferable but adding a dependency to a self-hosted service
// with no vendoring is a bigger operational change than it is worth here.
const (
	passwordScheme     = "pbkdf2-sha256"
	passwordIterations = 600000
	passwordSaltBytes  = 16
	passwordKeyBytes   = 32
	sessionTTL         = 12 * time.Hour
	authBodyLimit      = 4 << 10
)

var (
	accountNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
	sessionPattern     = regexp.MustCompile(`^ls_[A-Za-z0-9_-]{43}$`)
	apiTokenPattern    = regexp.MustCompile(`^gs_[A-Za-z0-9_-]{43}$`)

	errBadCredentials = errors.New("invalid credentials")
	errNotApproved    = errors.New("account not approved")
	errNoPassword     = errors.New("account has no password")
	errTooMany        = errors.New("too many attempts")
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, passwordKeyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", passwordScheme, passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// verifyPassword accepts only the current scheme and compares in constant time.
// Unknown or malformed encodings fail closed instead of falling back.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != passwordScheme {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 10000 || iterations > 10_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(want) != passwordKeyBytes {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validPassword(password string) bool {
	// Length only: composition rules push people toward predictable patterns,
	// and the real defence here is the rate limit plus the admin review.
	return len(password) >= 10 && len(password) <= 128
}

// authLimiter throttles register/login attempts per client and per account name.
// It is process-local: deploy a single instance, or move this to shared storage.
type authLimiter struct {
	mu     sync.Mutex
	window time.Time
	counts map[string]int
}

func (l *authLimiter) allow(key string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts == nil || now.Sub(l.window) >= 10*time.Minute {
		l.window, l.counts = now, make(map[string]int)
	}
	if l.counts[key] >= limit {
		return false
	}
	l.counts[key]++
	return true
}

func (l *authLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counts, key)
}

var accountGate = authLimiter{}

func clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	// Production puts nginx on the same host and it *overwrites* X-Real-IP with
	// the address it actually saw, so that header is trusted only when the
	// request truly arrives over the loopback proxy. Everywhere else the header
	// is caller-controlled and ignored: a forged value must not buy a fresh
	// rate-limit bucket. (X-Forwarded-For is deliberately not consulted: nginx
	// appends to whatever the client sent, so it is easy to get wrong.)
	if ip := net.ParseIP(peer); ip != nil && ip.IsLoopback() {
		if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil {
			return real.String()
		}
	}
	return peer
}

type loginSessionKey struct{}

// sessionUserID is non-zero only for a password-authenticated browser session,
// which may manage the account's own tokens but is not an API token.
func sessionUserID(r *http.Request) uint64 {
	v, _ := r.Context().Value(loginSessionKey{}).(uint64)
	return v
}

// AccountSummary is the presentation shape for /api/v1/me and the account page.
type AccountSummary struct {
	ID         uint64     `json:"id"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
}

// TokenView deliberately omits the digest: it must never leave the database.
type TokenView struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

func (s *Server) accountSummary(r *http.Request, id uint64) (AccountSummary, error) {
	var u User
	if err := s.DB.WithContext(r.Context()).Where("id = ?", id).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AccountSummary{}, errMissing
		}
		return AccountSummary{}, err
	}
	return AccountSummary{ID: u.ID, Name: u.Name, Status: u.Status, CreatedAt: u.CreatedAt, ApprovedAt: u.ApprovedAt}, nil
}

func (s *Server) tokensFor(userID uint64) ([]TokenView, error) {
	var tokens []Token
	if err := s.DB.Where("user_id = ?", userID).Order("created_at DESC, id ASC").Find(&tokens).Error; err != nil {
		return nil, err
	}
	views := make([]TokenView, 0, len(tokens))
	for _, t := range tokens {
		views = append(views, TokenView{ID: t.ID, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, RevokedAt: t.RevokedAt})
	}
	return views, nil
}

// decodeAuthBody bounds and strictly decodes an auth payload: unknown fields are
// rejected so a stray field can never be silently ignored.
func decodeAuthBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, authBodyLimit))
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			fail(w, 413, "request too large")
		} else {
			fail(w, 400, "incomplete request")
		}
		return false
	}
	if decodeJSON(b, v) != nil {
		fail(w, 400, "invalid request")
		return false
	}
	return true
}

func (s *Server) registerAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeAuthBody(w, r, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if !accountNamePattern.MatchString(name) || !validPassword(input.Password) {
		fail(w, 400, "invalid account name or password")
		return
	}
	now := time.Now().UTC()
	if !accountGate.allow("register:"+clientIP(r), 5, now) {
		fail(w, 429, "too many registration attempts")
		return
	}
	// Self-registration never grants access by itself: the account waits for an
	// administrator to approve it before it can issue API tokens.
	u, err := CreateUserWithPassword(s.DB.WithContext(r.Context()), name, input.Password, StatusPending)
	if err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "already exist") {
			fail(w, 409, "account name is already taken")
			return
		}
		fail(w, 503, "registration unavailable")
		return
	}
	jsonResponse(w, 201, map[string]any{"name": u.Name, "status": u.Status})
}

func (s *Server) loginAccount(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !decodeAuthBody(w, r, &input) {
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" || len(input.Password) == 0 || len(input.Password) > 128 {
		fail(w, 401, "invalid credentials")
		return
	}
	now := time.Now().UTC()
	ipKey, nameKey := "login:"+clientIP(r), "login-name:"+strings.ToLower(name)
	if !accountGate.allow(ipKey, 20, now) || !accountGate.allow(nameKey, 10, now) {
		fail(w, 429, "too many sign-in attempts")
		return
	}
	var u User
	err := s.DB.WithContext(r.Context()).Where("name = ?", name).First(&u).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// Spend comparable work on unknown names so timing does not confirm them.
		verifyPassword(dummyPasswordHash, input.Password)
		fail(w, 401, "invalid credentials")
		return
	case err != nil:
		fail(w, 503, "sign-in unavailable")
		return
	}
	if u.PasswordHash == "" || !verifyPassword(u.PasswordHash, input.Password) {
		fail(w, 401, "invalid credentials")
		return
	}
	if u.Status == StatusRejected {
		fail(w, 403, "account was rejected")
		return
	}
	raw, err := newSessionToken()
	if err != nil {
		fail(w, 503, "sign-in unavailable")
		return
	}
	session := LoginSession{Hash: tokenHash(raw), UserID: u.ID, ExpiresAt: now.Add(sessionTTL)}
	if err := s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND expires_at <= ?", u.ID, now).Delete(&LoginSession{}).Error; err != nil {
			return err
		}
		return tx.Create(&session).Error
	}); err != nil {
		fail(w, 503, "sign-in unavailable")
		return
	}
	accountGate.reset(nameKey)
	jsonResponse(w, 200, map[string]any{
		"name": u.Name, "status": u.Status, "sessionToken": raw, "expiresAt": session.ExpiresAt,
	})
}

// dummyPasswordHash keeps the unknown-name path as expensive as a real check.
var dummyPasswordHash = func() string {
	h, err := hashPassword("placeholder-password-value")
	if err != nil {
		return passwordScheme + "$600000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	return h
}()

func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ls_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func (s *Server) logoutAccount(w http.ResponseWriter, r *http.Request) {
	if sessionUserID(r) == 0 {
		fail(w, 401, "sign-in required")
		return
	}
	raw := bearerValue(r)
	if err := s.DB.WithContext(r.Context()).Where("hash = ?", tokenHash(raw)).Delete(&LoginSession{}).Error; err != nil {
		fail(w, 503, "sign-out unavailable")
		return
	}
	w.WriteHeader(204)
}

// unusablePasswordHash returns a fresh PBKDF2 encoding of a random value nobody
// knows. A closed account keeps that instead of an empty hash: the login path
// rejects an empty hash before spending any PBKDF2 work, so a closed name would
// answer measurably faster than an unknown one and confirm that it exists.
func unusablePasswordHash() (string, error) {
	b := make([]byte, passwordKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hashPassword(base64.RawURLEncoding.EncodeToString(b))
}

// DeleteAccount closes an account for good: every credential it owns is
// destroyed and the row itself stays behind as a tombstone. The name therefore
// stays taken (registering it again still conflicts) and games/leaderboard rows
// that point at the account keep pointing at a real user id.
//
// Games and scores are content, not credentials, so they are deliberately left
// alone: a closed account's public games stay playable and its scores stay on
// the boards. There is no recovery path (see ADR-0014).
func DeleteAccount(db *gorm.DB, userID uint64, password string) error {
	var u User
	if err := db.Where("id = ?", userID).First(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errMissing
		}
		return err
	}
	if u.Status == StatusDeleted || u.PasswordHash == "" || !verifyPassword(u.PasswordHash, password) {
		return errBadCredentials
	}
	replacement, err := unusablePasswordHash()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return db.Transaction(func(tx *gorm.DB) error {
		// Both credential kinds die here, plus the play and device sessions that
		// were minted from them. Nothing in this list is content.
		for _, model := range []any{&Token{}, &LoginSession{}, &PlaySession{}, &DeviceSession{}} {
			if err := tx.Where("user_id = ?", u.ID).Delete(model).Error; err != nil {
				return err
			}
		}
		// The status guard keeps two concurrent closes from both "succeeding".
		result := tx.Model(&User{}).Where("id = ? AND status <> ?", u.ID, StatusDeleted).Updates(map[string]any{
			"status":        StatusDeleted,
			"password_hash": replacement,
			"deleted_at":    now,
			"updated_at":    now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errMissing
		}
		return nil
	})
}

// closeMyAccount destroys the signed-in account. It needs a password login
// session -- an API token must never be able to delete the account it belongs
// to -- and the password again, so a stolen session on its own closes nothing.
// The account name has to be typed out as well; the two checks are independent.
func (s *Server) closeMyAccount(w http.ResponseWriter, r *http.Request) {
	if !requireSession(w, r) {
		return
	}
	var input struct {
		Password    string `json:"password"`
		ConfirmName string `json:"confirmName"`
	}
	if !decodeAuthBody(w, r, &input) {
		return
	}
	summary, err := s.accountSummary(r, sessionUserID(r))
	if errors.Is(err, errMissing) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "sign-in required")
		return
	}
	if err != nil {
		fail(w, 503, "close unavailable")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(input.ConfirmName), summary.Name) {
		fail(w, 400, "confirmation does not match the account name")
		return
	}
	// Rate-limit before touching the password, so the endpoint cannot be used to
	// brute-force it. The name key is what makes a single targeted account slow.
	now := time.Now().UTC()
	nameKey := "close-name:" + strings.ToLower(summary.Name)
	if !accountGate.allow("close:"+clientIP(r), 5, now) || !accountGate.allow(nameKey, 5, now) {
		fail(w, 429, "too many close attempts")
		return
	}
	if len(input.Password) == 0 || len(input.Password) > 128 {
		fail(w, 403, "password does not match")
		return
	}
	if err = DeleteAccount(s.DB.WithContext(r.Context()), summary.ID, input.Password); err != nil {
		switch {
		case errors.Is(err, errBadCredentials):
			fail(w, 403, "password does not match")
		case errors.Is(err, errMissing):
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, 401, "sign-in required")
		default:
			fail(w, 503, "close unavailable")
		}
		return
	}
	accountGate.reset(nameKey)
	w.WriteHeader(204)
}

// requireSession gates the token-management endpoints on a password login, so a
// leaked API token cannot mint further tokens.
func requireSession(w http.ResponseWriter, r *http.Request) bool {
	if sessionUserID(r) == 0 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "sign-in required")
		return false
	}
	return true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id := userID(r)
	if id == 0 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "token required")
		return
	}
	summary, err := s.accountSummary(r, id)
	if errors.Is(err, errMissing) {
		fail(w, 401, "token required")
		return
	}
	if err != nil {
		fail(w, 503, "account unavailable")
		return
	}
	jsonResponse(w, 200, summary)
}

func (s *Server) createMyToken(w http.ResponseWriter, r *http.Request) {
	if !requireSession(w, r) {
		return
	}
	var input struct {
		TTLHours *int `json:"ttlHours"`
	}
	if !decodeAuthBody(w, r, &input) {
		return
	}
	hours := 720
	if input.TTLHours != nil {
		hours = *input.TTLHours
	}
	if hours < 1 || hours > 8760 {
		fail(w, 400, "ttlHours must be between 1 and 8760")
		return
	}
	summary, err := s.accountSummary(r, sessionUserID(r))
	if err != nil {
		fail(w, 503, "account unavailable")
		return
	}
	if summary.Status != StatusActive {
		fail(w, 403, "account is waiting for administrator approval")
		return
	}
	token, raw, err := createTokenFor(s.DB.WithContext(r.Context()), summary.ID, time.Duration(hours)*time.Hour)
	if err != nil {
		fail(w, 503, "cannot issue token")
		return
	}
	// The plaintext token is returned exactly once and never logged or stored.
	jsonResponse(w, 201, map[string]any{"id": token.ID, "token": raw, "expiresAt": token.ExpiresAt})
}

func (s *Server) listMyTokens(w http.ResponseWriter, r *http.Request) {
	if !requireSession(w, r) {
		return
	}
	tokens, err := s.tokensFor(sessionUserID(r))
	if err != nil {
		fail(w, 503, "tokens unavailable")
		return
	}
	jsonResponse(w, 200, map[string]any{"tokens": tokens})
}

func (s *Server) revokeMyToken(w http.ResponseWriter, r *http.Request) {
	if !requireSession(w, r) {
		return
	}
	id := r.PathValue("id")
	if len(id) != 32 || !isHex(id) {
		fail(w, 404, "token not found")
		return
	}
	// Scope the update to the caller's own row: no cross-account revocation.
	result := s.DB.WithContext(r.Context()).Model(&Token{}).
		Where("id = ? AND user_id = ? AND revoked_at IS NULL", id, sessionUserID(r)).
		Update("revoked_at", time.Now().UTC())
	if result.Error != nil {
		fail(w, 503, "cannot revoke token")
		return
	}
	if result.RowsAffected == 0 {
		fail(w, 404, "token not found")
		return
	}
	w.WriteHeader(204)
}

func isHex(value string) bool {
	for _, c := range value {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
