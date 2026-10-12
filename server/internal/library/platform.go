package library

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Play credentials deliberately never populate the management identity context.
type sourceTokenKey struct{}

type PlaySession struct {
	Hash          string    `gorm:"size:64;primaryKey" json:"-"`
	GameID        string    `gorm:"size:64;index;not null"`
	UserID        uint64    `gorm:"index;not null"`
	SourceTokenID string    `gorm:"size:32;index;not null"`
	ExpiresAt     time.Time `gorm:"index;not null"`
	CreatedAt     time.Time
}

type PlayerScore struct {
	GameID     string    `gorm:"size:64;primaryKey"`
	UserID     uint64    `gorm:"primaryKey;autoIncrement:false"`
	Board      string    `gorm:"size:16;primaryKey"`
	Mode       string    `gorm:"size:64;primaryKey;not null;default:default"`
	Value      int64     `gorm:"not null"`
	AchievedAt time.Time `gorm:"not null;precision:6"`
}

// Hashing submission IDs preserves case-sensitive identity on MySQL's default
// case-insensitive collation. The idempotency namespace is player + game.
type ScoreSubmission struct {
	GameID         string    `gorm:"size:64;primaryKey"`
	UserID         uint64    `gorm:"primaryKey;autoIncrement:false"`
	SubmissionHash string    `gorm:"size:64;primaryKey"`
	Board          string    `gorm:"size:16;not null"`
	Mode           string    `gorm:"size:64;not null;default:default"`
	Value          int64     `gorm:"not null"`
	BestValue      int64     `gorm:"not null"`
	BestAchievedAt time.Time `gorm:"not null;precision:6"`
	CreatedAt      time.Time
}

type LeaderboardEntry struct {
	Rank       int       `json:"rank"`
	PlayerName string    `json:"playerName"`
	Value      int64     `json:"value"`
	AchievedAt time.Time `json:"achievedAt"`
}

var submissionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
var errPlayToken = errors.New("invalid play token")
var errSubmissionConflict = errors.New("submission conflict")
var errBoard = errors.New("invalid leaderboard parameters")

func validBoard(board string) bool { return board == "score" || board == "time-ms" }

func normalizedMode(mode string) string {
	if mode == "" {
		return "default"
	}
	return mode
}

func platformCORS(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 6 || parts[1] != "api" || parts[2] != "v1" || parts[3] != "games" || !idPattern.MatchString(parts[4]) {
		return false
	}
	method := ""
	switch parts[5] {
	case "scores":
		method = "POST"
	case "leaderboard":
		method = "GET"
	default:
		return false
	}
	if r.Method != method && r.Method != "OPTIONS" && !(method == "GET" && r.Method == "HEAD") {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != "OPTIONS" {
		return false
	}
	w.Header().Add("Vary", "Access-Control-Request-Method, Access-Control-Request-Headers")
	if r.Header.Get("Access-Control-Request-Method") != method {
		fail(w, 405, "unsupported CORS method")
		return true
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "", "content-type", "x-play-token":
		default:
			fail(w, 403, "unsupported CORS header")
			return true
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", method)
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Play-Token")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func (s *Server) createPlaySession(w http.ResponseWriter, r *http.Request) {
	if !writable(w, r) {
		return
	}
	// A play credential is re-validated against its source token on every
	// submission, so it must be minted from an API token. A browser login
	// session has no source token and would only produce a credential that can
	// never submit; refuse it here instead of handing out a dead session.
	if source, _ := r.Context().Value(sourceTokenKey{}).(string); source == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "account token required")
		return
	}
	g, ok := s.readable(w, r)
	if !ok {
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		fail(w, 503, "session unavailable")
		return
	}
	raw := "ps_" + base64.RawURLEncoding.EncodeToString(b)
	source, _ := r.Context().Value(sourceTokenKey{}).(string)
	p := PlaySession{Hash: tokenHash(raw), GameID: g.ID, UserID: userID(r), SourceTokenID: source, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := s.DB.WithContext(r.Context()).Create(&p).Error; err != nil {
		fail(w, 503, "session unavailable")
		return
	}
	jsonResponse(w, 201, map[string]any{"token": raw, "expiresAt": p.ExpiresAt, "gameId": p.GameID})
}

// Validate both expirations every time, so revocation of the source immediately
// invalidates future submissions even when the play session itself is unexpired.
func authenticatePlay(tx *gorm.DB, raw, gameID string) (PlaySession, error) {
	var p PlaySession
	if len(raw) != 46 || !strings.HasPrefix(raw, "ps_") {
		return p, errPlayToken
	}
	now := time.Now().UTC()
	err := tx.Where("hash = ? AND game_id = ? AND expires_at > ?", tokenHash(raw), gameID, now).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return p, errPlayToken
	}
	if err != nil {
		return p, err
	}
	var source Token
	err = tx.Where("id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?", p.SourceTokenID, p.UserID, now).First(&source).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return p, errPlayToken
	}
	return p, err
}

func (s *Server) submitScore(w http.ResponseWriter, r *http.Request) {
	gameID := r.PathValue("id")
	if !idPattern.MatchString(gameID) {
		fail(w, 400, "invalid game id")
		return
	}
	headers := r.Header.Values("X-Play-Token")
	if len(headers) != 1 {
		fail(w, 401, "play token required")
		return
	}
	// Bound and validate input before starting a transaction; a pointer rejects
	// missing/null values while int64 rejects fractional/overflowing numbers.
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		var max *http.MaxBytesError
		if errors.As(err, &max) {
			fail(w, 413, "score body too large")
		} else {
			fail(w, 400, "incomplete score body")
		}
		return
	}
	var input struct {
		Board        string `json:"board"`
		Mode         string `json:"mode"`
		Value        *int64 `json:"value"`
		SubmissionID string `json:"submissionId"`
	}
	if decodeJSON(b, &input) != nil || !validBoard(input.Board) || input.Value == nil || *input.Value < 0 || *input.Value > 9007199254740991 || (input.Board == "time-ms" && *input.Value == 0) || !submissionPattern.MatchString(input.SubmissionID) {
		fail(w, 400, "invalid score")
		return
	}
	var result ScoreSubmission
	input.Mode = normalizedMode(input.Mode)
	if !idPattern.MatchString(input.Mode) {
		fail(w, 400, "invalid mode")
		return
	}
	err = s.DB.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		// The existing game row is the serialization point, including the first
		// score for a player. No absent-row upsert race or lost best score.
		var g Game
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND deleted = ?", gameID, false).First(&g).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errMissing
		}
		if err != nil {
			return err
		}
		p, err := authenticatePlay(tx, headers[0], gameID)
		if err != nil {
			return err
		}
		if g.Visibility != "public" && g.OwnerID != p.UserID {
			return errMissing
		}
		key := ScoreSubmission{GameID: gameID, UserID: p.UserID, SubmissionHash: tokenHash(input.SubmissionID)}
		err = tx.Where(&key).First(&result).Error
		if err == nil {
			if result.Board != input.Board || result.Mode != input.Mode || result.Value != *input.Value {
				return errSubmissionConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var best PlayerScore
		err = tx.Where("game_id = ? AND user_id = ? AND board = ? AND mode = ?", gameID, p.UserID, input.Board, input.Mode).First(&best).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			best = PlayerScore{GameID: gameID, UserID: p.UserID, Board: input.Board, Mode: input.Mode, Value: *input.Value, AchievedAt: time.Now().UTC().Truncate(time.Microsecond)}
			if err = tx.Create(&best).Error; err != nil {
				return err
			}
		} else if (input.Board == "score" && *input.Value > best.Value) || (input.Board == "time-ms" && *input.Value < best.Value) {
			best.Value = *input.Value
			best.AchievedAt = time.Now().UTC().Truncate(time.Microsecond)
			if err = tx.Model(&best).Updates(map[string]any{"value": best.Value, "achieved_at": best.AchievedAt}).Error; err != nil {
				return err
			}
		}
		result = key
		result.Board, result.Value = input.Board, *input.Value
		result.Mode = input.Mode
		result.BestValue, result.BestAchievedAt = best.Value, best.AchievedAt
		return tx.Create(&result).Error
	})
	if err != nil {
		switch {
		case errors.Is(err, errPlayToken):
			fail(w, 401, "invalid play token")
		case errors.Is(err, errMissing):
			fail(w, 404, "game not found")
		case errors.Is(err, errSubmissionConflict):
			fail(w, 409, "submission id already used with different payload")
		default:
			fail(w, 503, "score unavailable")
		}
		return
	}
	jsonResponse(w, 200, map[string]any{"gameId": gameID, "board": result.Board, "mode": result.Mode, "value": result.BestValue, "achievedAt": result.BestAchievedAt})
}

// mode 由调用方**显式传入**，不再从这里偷看 query。
//
// 以前它自己 `r.URL.Query().Get("mode")`，于是"页面决定显示哪个模式"和"条目按哪个模式查"
// 是两条独立的路径：页面把默认模式解析成有成绩的那个，条目却仍按 query 里的（缺失即
// "default"）去查——标题和内容对不上，用户看到的是空榜。
func (s *Server) leaderboardEntries(r *http.Request, gameID, board, mode string, limit int) ([]LeaderboardEntry, error) {
	if len(r.URL.Query()["mode"]) > 1 || !idPattern.MatchString(mode) || !idPattern.MatchString(gameID) || !validBoard(board) || limit < 1 || limit > 100 {
		return nil, errBoard
	}
	db := s.DB.WithContext(r.Context())
	// One query joins visibility and rows: no separate authorization/read window
	// can publish scores after the game was made private or deleted.
	var g Game
	err := db.Where("id = ? AND deleted = ? AND (visibility = ? OR owner_id = ?)", gameID, false, "public", userID(r)).First(&g).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, errMissing
	}
	if err != nil {
		return nil, err
	}
	direction := "DESC"
	if board == "time-ms" {
		direction = "ASC"
	}
	entries := make([]LeaderboardEntry, 0)
	err = db.Table("player_scores AS p").Select("u.name AS player_name, p.value, p.achieved_at").
		Joins("JOIN users AS u ON u.id = p.user_id").
		Joins("JOIN games AS g ON g.id = p.game_id").
		Where("p.game_id = ? AND p.board = ? AND p.mode = ? AND g.deleted = ? AND (g.visibility = ? OR g.owner_id = ?)", gameID, board, mode, false, "public", userID(r)).
		Order("p.value " + direction + ", p.achieved_at ASC, p.user_id ASC").Limit(limit).Scan(&entries).Error
	if err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Rank = i + 1
		if i > 0 && entries[i].Value == entries[i-1].Value {
			entries[i].Rank = entries[i-1].Rank
		}
	}
	return entries, nil
}

func (s *Server) leaderboard(w http.ResponseWriter, r *http.Request) {
	board := r.URL.Query().Get("board")
	if board == "" {
		board = "score"
	}
	limit := 20
	if value, ok := r.URL.Query()["limit"]; ok {
		var err error
		if len(value) != 1 {
			fail(w, 400, "invalid limit")
			return
		}
		limit, err = strconv.Atoi(value[0])
		if err != nil {
			fail(w, 400, "invalid limit")
			return
		}
	}
	entries, err := s.leaderboardEntries(r, r.PathValue("id"), board, normalizedMode(r.URL.Query().Get("mode")), limit)
	if err != nil {
		switch {
		case errors.Is(err, errBoard):
			fail(w, 400, "invalid leaderboard parameters")
		case errors.Is(err, errMissing):
			fail(w, 404, "game not found")
		default:
			fail(w, 503, "leaderboard unavailable")
		}
		return
	}
	jsonResponse(w, 200, map[string]any{"gameId": r.PathValue("id"), "board": board, "mode": normalizedMode(r.URL.Query().Get("mode")), "entries": entries})
}
