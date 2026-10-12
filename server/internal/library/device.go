package library

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DeviceSession never contains a plaintext device secret or play credential.
type DeviceSession struct {
	Hash          string `gorm:"size:64;primaryKey" json:"-"`
	UserCode      string `gorm:"size:8;uniqueIndex;not null"`
	GameID        string `gorm:"size:64;index;not null"`
	UserID        uint64
	SourceTokenID string     `gorm:"size:32"`
	ExpiresAt     time.Time  `gorm:"index;not null;precision:6"`
	LastPolledAt  *time.Time `gorm:"precision:6"`
	CreatedAt     time.Time
}

const deviceAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
const devicePollInterval = 5 * time.Second

// Capacity and creation rate limits are atomic ONLY inside this process. Deploy
// one instance; multiple instances require a shared limiter/DB serialization.
// Entries expire every minute and the global limit bounds the IP map to 120.
var deviceGate = struct {
	sync.Mutex
	window time.Time
	count  int
	ips    map[string]int
}{}

var errDeviceGone = errors.New("device session expired or consumed")
var errDeviceConflict = errors.New("device already authorized")
var errDeviceCapacity = errors.New("device capacity reached")

//go:embed device_connect.html
var deviceConnectHTML string

//go:embed device_connect.js
var deviceConnectJS []byte
var deviceConnectTemplate = template.Must(template.New("device-connect").Parse(deviceConnectHTML))

func deviceHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func deviceBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		fail(w, 413, "device request too large or incomplete")
		return false
	}
	if decodeJSON(b, v) != nil {
		fail(w, 400, "invalid device request")
		return false
	}
	return true
}

func validDeviceCode(code string) bool {
	if len(code) != 8 {
		return false
	}
	for _, c := range code {
		if !strings.ContainsRune(deviceAlphabet, c) {
			return false
		}
	}
	return true
}

func deviceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errMissing), errors.Is(err, gorm.ErrRecordNotFound):
		fail(w, 404, "device code or public game not found")
	case errors.Is(err, errDeviceGone):
		fail(w, 410, "device session expired or consumed; pair again")
	case errors.Is(err, errDeviceConflict):
		fail(w, 409, "device already authorized")
	case errors.Is(err, errPlayToken):
		fail(w, 401, "authorization no longer valid; pair again")
	case errors.Is(err, errDeviceCapacity):
		w.Header().Set("Retry-After", "60")
		fail(w, 429, "device pairing capacity reached")
	default:
		fail(w, 503, "device pairing unavailable")
	}
}

func publicDeviceGame(tx *gorm.DB, id string) (Game, error) {
	var g Game
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND visibility = ? AND deleted = ?", id, "public", false).First(&g).Error
	return g, err
}

func (s *Server) createDeviceSession(w http.ResponseWriter, r *http.Request) {
	deviceHeaders(w)
	id := r.PathValue("id")
	if !idPattern.MatchString(id) {
		fail(w, 400, "invalid game id")
		return
	}
	// No payload is needed; allow {} for JSON-only clients but reject other data.
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		fail(w, 413, "device request too large or incomplete")
		return
	}
	if len(strings.TrimSpace(string(b))) != 0 && strings.TrimSpace(string(b)) != "{}" {
		fail(w, 400, "device request must be empty")
		return
	}
	deviceGate.Lock()
	defer deviceGate.Unlock()
	now := time.Now().UTC()
	if deviceGate.window.IsZero() || now.Sub(deviceGate.window) >= time.Minute {
		deviceGate.window, deviceGate.count, deviceGate.ips = now, 0, make(map[string]int)
	}
	ip := clientIP(r)
	if deviceGate.count >= 120 || deviceGate.ips[ip] >= 20 {
		deviceError(w, errDeviceCapacity)
		return
	}
	deviceGate.count++
	deviceGate.ips[ip]++
	secret := make([]byte, 32)
	codeBytes := make([]byte, 8)
	if _, err = rand.Read(secret); err != nil {
		deviceError(w, err)
		return
	}
	if _, err = rand.Read(codeBytes); err != nil {
		deviceError(w, err)
		return
	}
	for i := range codeBytes {
		codeBytes[i] = deviceAlphabet[int(codeBytes[i])&31]
	}
	raw := "gd_" + base64.RawURLEncoding.EncodeToString(secret)
	d := DeviceSession{Hash: tokenHash(raw), UserCode: string(codeBytes), GameID: id, ExpiresAt: now.Add(5 * time.Minute)}
	err = s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&DeviceSession{}).Error; err != nil {
			return err
		}
		if _, err := publicDeviceGame(tx, id); err != nil {
			return err
		}
		var total, perGame int64
		if err := tx.Model(&DeviceSession{}).Count(&total).Error; err != nil {
			return err
		}
		if err := tx.Model(&DeviceSession{}).Where("game_id = ?", id).Count(&perGame).Error; err != nil {
			return err
		}
		if total >= 100 || perGame >= 10 {
			return errDeviceCapacity
		}
		return tx.Create(&d).Error
	})
	if err != nil {
		deviceError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]any{"deviceToken": raw, "userCode": d.UserCode, "verificationUrl": s.BaseURL + "/device-connect?code=" + d.UserCode, "expiresAt": d.ExpiresAt, "pollInterval": 5})
}

func (s *Server) authorizeDeviceSession(w http.ResponseWriter, r *http.Request) {
	deviceHeaders(w)
	source, _ := r.Context().Value(sourceTokenKey{}).(string)
	if userID(r) == 0 || source == "" {
		// Distinguish "no credential" from "the one you sent is no good": saying
		// "account token required" to someone who did send one sends them looking
		// in the wrong place.
		if badCredential(r) {
			fail(w, 401, "invalid token")
			return
		}
		fail(w, 401, "account token required")
		return
	}
	var input struct {
		UserCode string `json:"userCode"`
	}
	if !deviceBody(w, r, &input) {
		return
	}
	if !validDeviceCode(input.UserCode) {
		deviceError(w, errMissing)
		return
	}
	deviceGate.Lock()
	defer deviceGate.Unlock()
	err := s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var d DeviceSession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_code = ? AND expires_at > ?", input.UserCode, now).First(&d).Error; err != nil {
			return err
		}
		if _, err := publicDeviceGame(tx, d.GameID); err != nil {
			return err
		}
		if d.UserID != 0 || d.SourceTokenID != "" {
			return errDeviceConflict
		}
		var u User
		if err := tx.First(&u, userID(r)).Error; err != nil {
			return err
		}
		var token Token
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", source, u.ID, now).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errPlayToken
			}
			return err
		}
		return tx.Model(&d).Updates(map[string]any{"user_id": u.ID, "source_token_id": source}).Error
	})
	if err != nil {
		deviceError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]bool{"authorized": true})
}

func (s *Server) pollDeviceSession(w http.ResponseWriter, r *http.Request) {
	deviceHeaders(w)
	var input struct {
		DeviceToken string `json:"deviceToken"`
	}
	if !deviceBody(w, r, &input) {
		return
	}
	if !idPattern.MatchString(r.PathValue("id")) {
		fail(w, 400, "invalid game id")
		return
	}
	if len(input.DeviceToken) != 46 || !strings.HasPrefix(input.DeviceToken, "gd_") {
		deviceError(w, errDeviceGone)
		return
	}
	deviceGate.Lock()
	defer deviceGate.Unlock()
	status := 202
	var raw string
	var p PlaySession
	err := s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var d DeviceSession
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("hash = ? AND game_id = ?", tokenHash(input.DeviceToken), r.PathValue("id")).First(&d).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errDeviceGone
		}
		if err != nil {
			return err
		}
		if !d.ExpiresAt.After(now) {
			status = 410
			return tx.Delete(&d).Error
		}
		if d.LastPolledAt != nil && now.Sub(*d.LastPolledAt) < devicePollInterval {
			status = 429
			return nil
		}
		if err := tx.Model(&d).Update("last_polled_at", now).Error; err != nil {
			return err
		}
		if _, err := publicDeviceGame(tx, d.GameID); err != nil {
			return err
		}
		if d.UserID == 0 {
			return nil
		}
		var source Token
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", d.SourceTokenID, d.UserID, now).First(&source).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errPlayToken
		}
		if err != nil {
			return err
		}
		var u User
		if err := tx.First(&u, d.UserID).Error; err != nil {
			return err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		raw = "ps_" + base64.RawURLEncoding.EncodeToString(b)
		p = PlaySession{Hash: tokenHash(raw), GameID: d.GameID, UserID: d.UserID, SourceTokenID: source.ID, ExpiresAt: now.Add(time.Hour)}
		if source.ExpiresAt.Before(p.ExpiresAt) {
			p.ExpiresAt = source.ExpiresAt
		}
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		if err := tx.Delete(&d).Error; err != nil {
			return err
		}
		status = 200
		return nil
	})
	if err != nil {
		deviceError(w, err)
		return
	}
	switch status {
	case 200:
		// Commit/delete precedes the one-time response. A lost response requires
		// a new pairing; no plaintext ps credential is retained for retries.
		jsonResponse(w, 200, map[string]any{"token": raw, "gameId": p.GameID, "expiresAt": p.ExpiresAt})
	case 410:
		deviceError(w, errDeviceGone)
	case 429:
		w.Header().Set("Retry-After", "5")
		fail(w, 429, "poll no more than once every 5 seconds")
	default:
		w.Header().Set("Retry-After", "5")
		jsonResponse(w, 202, map[string]string{"status": "pending"})
	}
}

func (s *Server) deviceConnectPage(w http.ResponseWriter, r *http.Request) {
	deviceHeaders(w)
	code := r.URL.Query().Get("code")
	if len(r.URL.Query()["code"]) != 1 || !validDeviceCode(code) {
		deviceError(w, errMissing)
		return
	}
	// A single joined query checks TTL and visibility; no user/source identity
	// is selected or exposed, even after authorization.
	var rows []struct{ Metadata string }
	err := s.DB.WithContext(r.Context()).Table("device_sessions AS d").Select("g.metadata").Joins("JOIN games AS g ON g.id = d.game_id").Where("d.user_code = ? AND d.expires_at > ? AND g.visibility = ? AND g.deleted = ?", code, time.Now().UTC(), "public", false).Limit(1).Scan(&rows).Error
	if err != nil {
		deviceError(w, err)
		return
	}
	if len(rows) != 1 {
		deviceError(w, errMissing)
		return
	}
	var metadata Metadata
	if json.Unmarshal([]byte(rows[0].Metadata), &metadata) != nil {
		fail(w, 503, "device pairing unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = deviceConnectTemplate.Execute(w, map[string]string{"Title": metadata.Title, "Code": code})
}

func (s *Server) deviceConnectScript(w http.ResponseWriter, r *http.Request) {
	deviceHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(deviceConnectJS)
}
