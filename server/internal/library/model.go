package library

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type User struct {
	ID   uint64 `gorm:"primaryKey" json:"id"`
	Name string `gorm:"size:128;uniqueIndex;not null" json:"name"`
	// Empty for accounts provisioned by the admin CLI without a password: those
	// can still use tokens, but cannot sign in to issue new ones.
	PasswordHash string `gorm:"size:255;not null;default:''" json:"-"`
	// active | pending | rejected. Admin-created accounts are active immediately;
	// self-registration always starts pending until an administrator approves it.
	Status     string     `gorm:"size:16;index;not null;default:'active'" json:"status"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	// 注销时间。**不是** `gorm.DeletedAt`：注销只是"这个账号不能再用了"，
	// 行本身必须留下当墓碑（名字继续被占、games.owner_id / player_scores.user_id
	// 仍然指向一个真实用户）。真用软删除会把它从排行榜的 JOIN 里藏掉。
	DeletedAt *time.Time `gorm:"index" json:"deletedAt,omitempty"`
}

// LoginSession is a password-authenticated browser session. Only the digest is
// stored, and it can only manage the account's own API tokens -- never game or
// token operations that require an API token.
type LoginSession struct {
	Hash      string    `gorm:"size:64;primaryKey" json:"-"`
	UserID    uint64    `gorm:"index;not null" json:"userId"`
	ExpiresAt time.Time `gorm:"index;not null" json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
}

const (
	StatusActive   = "active"
	StatusPending  = "pending"
	StatusRejected = "rejected"
	// StatusDeleted 是终态：账号已注销，凭据全部销毁，不能登录、不能签发
	// Token，也不提供自助恢复（见 ADR-0014）。它**不在** validStatus 里——
	// 管理员审核（approve/reject/create --status）不能把账号设成注销。
	StatusDeleted = "deleted"
)

// validStatus 只覆盖管理员审核可写入的状态；StatusDeleted 只能由
// DeleteAccount 写入。
func validStatus(status string) bool {
	return status == StatusActive || status == StatusPending || status == StatusRejected
}

type Token struct {
	ID        string     `gorm:"size:32;primaryKey" json:"id"`
	UserID    uint64     `gorm:"index;not null" json:"userId"`
	Hash      string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	ExpiresAt time.Time  `gorm:"index;not null" json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}
type Game struct {
	ID         string `gorm:"size:64;primaryKey"`
	OwnerID    uint64 `gorm:"index;not null"`
	Visibility string `gorm:"size:8;index;not null"`
	Metadata   string `gorm:"type:text;not null"`
	Filename   string `gorm:"size:80;not null"`
	// 包体在对象存储里的 key。非空表示这个包**只在 OSS**（本地没有副本）；
	// 空表示老记录，仍从 DATA_DIR/<Filename> 提供。
	PackageKey string `gorm:"type:text"`
	Size       int64
	SHA256     string `gorm:"size:64"`
	// **收到**的字节（容器提交时就是 .game 容器本身），与"对外分发的东西"（pck）区分开。
	// 上传方要靠它核对"服务端确实收到了我发的那些字节"，而清单里的 package.* 描述的是
	// 客户端会下载到的那个对象——两者在容器提交时**本来就不是一个东西**。
	ReceivedSize   int64
	ReceivedSHA256 string `gorm:"size:64"`
	// 分发格式：`pck`（旧形态，裸资源包）或 `game/1`（.game 容器）。
	// 老记录一律是 pck，所以默认值就是它。
	Format string `gorm:"size:16;not null;default:'pck'"`
	// 从容器里抽出来的素材（JSON，见 GameAssets）。
	// 刻意**不加** `not null` 与 `default`：MySQL 不允许 TEXT 列有默认值（Error 1101），
	// 而给既有表加「NOT NULL 且无默认值」的列在严格模式下同样会失败。
	// 沿用仓库里既有的做法：列可空，Migrate 之后把 NULL 回填成空串，
	// 这样读取端永远看不到 NULL（见 Migrate 末尾的回填）。
	Assets string `gorm:"type:text"`
	// 游戏本体出处（JSON，见 BundleSource）：仓库地址、ref、commit、许可证。
	// 平台只记录、不拉取（ADR-0011）。
	Source    string `gorm:"type:text"`
	Deleted   bool   `gorm:"index;not null;default:false"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
type Engine struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type Metadata struct {
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Accent      string   `json:"accent,omitempty"`
	Engine      Engine   `json:"engine"`
	Visibility  string   `json:"visibility,omitempty"`
	// 模式 id → 显示名。来自容器元数据的 modes 声明，**不参与** Validate（老记录没有它）。
	Modes []BundleMode `json:"modes,omitempty"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var versionPattern = regexp.MustCompile(`^4\.[0-9]{1,3}$`)
var accentPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (m Metadata) Validate() error {
	if len(m.Title) == 0 || len(m.Title) > 512 || len(m.Subtitle) > 1024 || len(m.Description) > 8192 || len(m.Tags) > 16 {
		return errors.New("invalid metadata lengths")
	}
	for _, v := range m.Tags {
		if len(v) > 128 {
			return errors.New("tag too long")
		}
	}
	if m.Engine.Name != "godot" || !versionPattern.MatchString(m.Engine.Version) {
		return errors.New("invalid engine")
	}
	if m.Accent != "" && !accentPattern.MatchString(m.Accent) {
		return errors.New("invalid accent")
	}
	if m.Visibility != "" && m.Visibility != "private" && m.Visibility != "public" {
		return errors.New("invalid visibility")
	}
	return nil
}
func OpenMySQL(dsn string) (*gorm.DB, error) {
	if dsn == "" {
		return nil, errors.New("MYSQL_DSN is required")
	}
	// Never log SQL parameters (including token digests), DSNs, or driver errors.
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return nil, errors.New("invalid MYSQL_DSN")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent), TranslateError: true, NowFunc: func() time.Time { return time.Now().UTC() }})
	if err != nil {
		return nil, errors.New("database connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, errors.New("database unavailable")
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}
func Migrate(db *gorm.DB) error {
	if err := migrateScoreModes(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(&User{}, &Token{}, &Game{}, &PlaySession{}, &PlayerScore{}, &ScoreSubmission{}, &DeviceSession{}, &LoginSession{}); err != nil {
		return err
	}
	// Accounts created before self-registration existed were admin-provisioned,
	// so they are active by definition; never leave them with an empty status.
	if err := db.Model(&User{}).Where("status IS NULL OR status = ?", "").Update("status", StatusActive).Error; err != nil {
		return err
	}
	// games.assets / games.source 是可空 TEXT（MySQL 不允许 TEXT 有默认值），
	// 这里把既有的 NULL 回填成空串，读取端就永远不用处理 NULL。
	for _, column := range []string{"assets", "source"} {
		if err := db.Model(&Game{}).Where(column+" IS NULL").Update(column, "").Error; err != nil {
			return err
		}
	}
	return nil
}
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func tokenHash(raw string) string { s := sha256.Sum256([]byte(raw)); return hex.EncodeToString(s[:]) }
func CreateUser(db *gorm.DB, name string) (User, error) {
	u := User{Name: name}
	if name == "" || len(name) > 128 {
		return u, errors.New("name must be 1-128 bytes")
	}
	u.Status = StatusActive
	if err := db.Create(&u).Error; err != nil {
		return User{}, errors.New("cannot create user (name may already exist)")
	}
	return u, nil
}

// SetUserStatus moves an account through admin review. Approving stamps the time
// so the account page can show when access was granted; rejecting clears it.
func SetUserStatus(db *gorm.DB, name, status string) (User, error) {
	var u User
	if !validStatus(status) {
		return u, errors.New("status must be active, pending or rejected")
	}
	if err := db.Where("name = ?", name).First(&u).Error; err != nil {
		return u, errors.New("user not found")
	}
	now := time.Now().UTC()
	updates := map[string]any{"status": status, "updated_at": now}
	if status == StatusActive {
		updates["approved_at"] = now
	} else {
		updates["approved_at"] = nil
	}
	if err := db.Model(&User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
		return User{}, errors.New("cannot update user status")
	}
	u.Status = status
	// Keep the returned struct consistent with the row we just wrote: this value
	// is what the admin CLI prints as the audit trail.
	u.UpdatedAt = now
	if status == StatusActive {
		u.ApprovedAt = &now
	} else {
		u.ApprovedAt = nil
	}
	return u, nil
}

func PendingUsers(db *gorm.DB) ([]User, error) {
	var users []User
	if err := db.Where("status = ?", StatusPending).Order("id ASC").Find(&users).Error; err != nil {
		return nil, errors.New("cannot list pending users")
	}
	return users, nil
}

func createTokenFor(db *gorm.DB, userID uint64, ttl time.Duration) (Token, string, error) {
	if ttl <= 0 || ttl > 365*24*time.Hour {
		return Token{}, "", errors.New("ttl must be positive and at most 8760h")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Token{}, "", err
	}
	raw := "gs_" + base64.RawURLEncoding.EncodeToString(b)
	id, err := randomHex(16)
	if err != nil {
		return Token{}, "", err
	}
	t := Token{ID: id, UserID: userID, Hash: tokenHash(raw), ExpiresAt: time.Now().UTC().Add(ttl)}
	if err = db.Create(&t).Error; err != nil {
		return Token{}, "", errors.New("cannot create token")
	}
	return t, raw, nil
}

func CreateToken(db *gorm.DB, name string, ttl time.Duration) (Token, string, error) {
	if ttl <= 0 || ttl > 365*24*time.Hour {
		return Token{}, "", errors.New("ttl must be positive and at most 8760h")
	}
	var u User
	if err := db.Where("name = ?", name).First(&u).Error; err != nil {
		return Token{}, "", errors.New("user not found")
	}
	return createTokenFor(db, u.ID, ttl)
}

// CreateUserWithPassword provisions an account with a password and an explicit
// review status. The admin CLI and the self-registration handler share it.
func CreateUserWithPassword(db *gorm.DB, name, password, status string) (User, error) {
	u := User{Name: name}
	if name == "" || len(name) > 128 {
		return u, errors.New("name must be 1-128 bytes")
	}
	if !validStatus(status) {
		return u, errors.New("invalid status")
	}
	hash := ""
	if password != "" {
		var err error
		hash, err = hashPassword(password)
		if err != nil {
			return User{}, err
		}
	}
	u = User{Name: name, PasswordHash: hash, Status: status}
	if status == StatusActive {
		now := time.Now().UTC()
		u.ApprovedAt = &now
	}
	if err := db.Create(&u).Error; err != nil {
		return User{}, errors.New("cannot create user (name may already exist)")
	}
	return u, nil
}
func RevokeToken(db *gorm.DB, id string) error {
	r := db.Model(&Token{}).Where("id = ?", id).Update("revoked_at", time.Now().UTC())
	if r.Error != nil {
		return errors.New("cannot revoke token")
	}
	if r.RowsAffected == 0 {
		return fmt.Errorf("token not found")
	}
	return nil
}
