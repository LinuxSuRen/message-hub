// Package model 定义持久化数据模型与对外传输对象（DTO）。
package model

import "time"

// 角色取值。
const (
	RoleAdmin        = "admin"         // 平台管理员
	RoleTenantAdmin  = "tenant_admin"  // 租户管理员
	RoleUser         = "user"          // 普通用户
)

// 消息级别取值。
const (
	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
)

// Levels 返回合法的消息级别。
func Levels() []string {
	return []string{LevelInfo, LevelWarning, LevelError}
}

// Tenant 租户。
type Tenant struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:64;uniqueIndex;not null" json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// User 用户。
type User struct {
	ID           uint64 `gorm:"primaryKey" json:"id"`
	TenantID     uint64 `gorm:"index;not null" json:"tenant_id"`
	Tenant       *Tenant `gorm:"foreignKey:TenantID" json:"-"`
	Email        string `gorm:"size:255;uniqueIndex;not null" json:"email"`
	PasswordHash string `gorm:"size:255" json:"-"` // OAuth 账号可能为空
	DisplayName  string `gorm:"size:64" json:"display_name"`
	Role         string `gorm:"size:32;index;not null;default:user" json:"role"`
	// SessionEpoch 会话世代：重置密码等安全事件时 +1，使该用户全部已签发会话立即失效。
	SessionEpoch uint64 `gorm:"not null;default:0" json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// OAuthIdentity 第三方登录身份（provider + provider_uid 唯一）。
type OAuthIdentity struct {
	ID          uint64 `gorm:"primaryKey" json:"id"`
	UserID      uint64 `gorm:"index;not null" json:"user_id"`
	Provider    string `gorm:"size:32;uniqueIndex:idx_oauth_provider_uid" json:"provider"`
	ProviderUID string `gorm:"size:128;uniqueIndex:idx_oauth_provider_uid" json:"provider_uid"`
	CreatedAt   time.Time `json:"created_at"`
}

// Token 推送令牌（只存哈希，明文仅创建时返回一次）。
type Token struct {
	ID              uint64 `gorm:"primaryKey" json:"id"`
	UserID          uint64 `gorm:"index;not null" json:"user_id"`
	Name            string `gorm:"size:64" json:"name"`
	TokenHash       string `gorm:"size:64;uniqueIndex;not null" json:"-"`
	RateLimitPerMin int    `gorm:"not null;default:60" json:"rate_limit_per_min"` // 每分钟推送上限
	LastUsedAt      *time.Time `json:"last_used_at"`
	RevokedAt       *time.Time `json:"-"`
	CreatedAt       time.Time `json:"created_at"`
}

// Message 消息。SourceID 非零表示来自外部信息源，仅推送给该源的订阅者。
type Message struct {
	ID        uint64 `gorm:"primaryKey" json:"id"`
	TenantID  uint64 `gorm:"index;not null" json:"tenant_id"`
	UserID    uint64 `gorm:"index" json:"user_id"`
	SourceID  uint64 `gorm:"index" json:"source_id,omitempty"`
	Topic     string `gorm:"size:64;index" json:"topic"`
	Title     string `gorm:"size:128;not null" json:"title"`
	Body      string `gorm:"size:4096" json:"body"`
	Level     string `gorm:"size:16;not null;default:info" json:"level"`
	Public    bool   `gorm:"index" json:"public"`
	CreatedAt time.Time `json:"created_at"`
}

// Source 外部信息源（如 GitHub 仓库监视）。同一仓库只允许一个源。
// 拉取凭证（PAT）与 webhook 验签密钥均以 AES-GCM 加密落库。
type Source struct {
	ID        uint64 `gorm:"primaryKey" json:"id"`
	TenantID  uint64 `gorm:"index;not null" json:"tenant_id"`
	UserID    uint64 `gorm:"index;not null" json:"user_id"`
	Type      string `gorm:"size:32;not null;default:github;uniqueIndex:idx_source_target" json:"type"`
	Owner     string `gorm:"size:64;not null;uniqueIndex:idx_source_target" json:"owner"`
	Repo      string `gorm:"size:128;not null;uniqueIndex:idx_source_target" json:"repo"`
	TokenEnc  string `gorm:"size:1024" json:"-"`
	SecretEnc string `gorm:"size:1024" json:"-"`
	WebhookID int64  `gorm:"index" json:"-"`
	// 关注内容：release/issues/pull 走 webhook 实时推送，stars/forks 走轮询。
	// 注意：bool 一律 default:false——gorm 会把零值替换为列默认值，
	// default:true 会把“调用方未勾选（false）”静默改成 true。
	WatchRelease bool `gorm:"not null;default:false" json:"watch_release"`
	WatchIssues  bool `gorm:"not null;default:false" json:"watch_issues"`
	WatchPulls   bool `gorm:"not null;default:false" json:"watch_pulls"`
	WatchStars   bool `gorm:"not null;default:false" json:"watch_stars"`
	WatchForks   bool `gorm:"not null;default:false" json:"watch_forks"`
	// UseWebhook 是否在 GitHub 侧注册 webhook（需要对外可达地址）。
	// 关闭时源照常创建，star/fork 轮询不受影响；由用户自行决定。
	UseWebhook bool `gorm:"not null;default:false" json:"use_webhook"`
	// IntervalSec star/fork 轮询间隔（秒）。
	IntervalSec int `gorm:"not null;default:300" json:"interval_sec"`
	// LastStars/LastForks 上次观测值；-1 表示尚未建立基线。
	LastStars int64 `gorm:"not null;default:-1" json:"last_stars"`
	LastForks int64 `gorm:"not null;default:-1" json:"last_forks"`
	LastRunAt *time.Time `json:"last_run_at"`
	NextRunAt time.Time  `gorm:"index" json:"-"`
	LastError string     `gorm:"size:512" json:"last_error"`
	Enabled   bool       `gorm:"not null;default:true" json:"enabled"`
	CreatedAt time.Time  `json:"created_at"`
}

// Topic 消息源对应的主题（客户端按此过滤）；钳制到消息表 64 字上限。
func (s *Source) Topic() string {
	topic := s.Type + "/" + s.Owner + "/" + s.Repo
	if runes := []rune(topic); len(runes) > 64 {
		return string(runes[:64])
	}
	return topic
}

// NeedWebhook 是否有 webhook 类关注内容（release/issues/pull 任一开启）。
// 是否真的注册 webhook 还取决于 UseWebhook（用户决定）。
func (s *Source) NeedWebhook() bool {
	return s.WatchRelease || s.WatchIssues || s.WatchPulls
}

// WantWebhook 注册条件：用户开启同步 且 有 webhook 类关注内容。
func (s *Source) WantWebhook() bool {
	return s.UseWebhook && s.NeedWebhook()
}

// WebhookEvents 按关注开关返回需要注册的 webhook 事件列表。
func (s *Source) WebhookEvents() []string {
	events := []string{}
	if s.WatchRelease {
		events = append(events, "release")
	}
	if s.WatchIssues {
		events = append(events, "issues")
	}
	if s.WatchPulls {
		events = append(events, "pull_request")
	}
	return events
}

// SourceSub 用户对消息源的订阅。
type SourceSub struct {
	ID        uint64 `gorm:"primaryKey" json:"id"`
	SourceID  uint64 `gorm:"uniqueIndex:idx_source_sub_target" json:"source_id"`
	UserID    uint64 `gorm:"uniqueIndex:idx_source_sub_target;index" json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// SourceItem 消息源条目去重账本：external_id 唯一，防 webhook 重投/补拉重复转存。
type SourceItem struct {
	ID         uint64 `gorm:"primaryKey" json:"id"`
	SourceID   uint64 `gorm:"uniqueIndex:idx_source_item_target" json:"source_id"`
	ExternalID string `gorm:"size:255;uniqueIndex:idx_source_item_target" json:"-"`
	MessageID  uint64 `json:"message_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// Setting 系统设置（键值对，当前仅对外地址 base_url）。
type Setting struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `gorm:"size:512" json:"value"`
}

// 系统设置键。
const (
	SettingBaseURL = "base_url" // 对外基础地址，webhook 回调使用；空则回落 -base-url
)

// VerifyCode 邮箱验证码（只存哈希）。
type VerifyCode struct {
	ID        uint64 `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;index;not null" json:"email"`
	CodeHash  string `gorm:"size:64;not null" json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    *time.Time `json:"-"`
	// FailedAttempts 连续校验失败次数；达到 MaxCodeAttempts 后验证码立即作废（防穷举）。
	FailedAttempts int `gorm:"not null;default:0" json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// MaxCodeAttempts 验证码最大连续失败次数。
const MaxCodeAttempts = 5

// TenantDTO 租户对外视图。
type TenantDTO struct {
	ID        uint64 `json:"id"`
	Name      string `json:"name"`
	UserCount int64  `json:"user_count,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// UserDTO 用户对外视图（不含敏感字段）。
type UserDTO struct {
	ID          uint64    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	Tenant      TenantRef `json:"tenant"`
	CreatedAt   time.Time `json:"created_at"`
}

// TenantRef 用户视图中的租户引用。
type TenantRef struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

// SourceDTO 消息源对外视图（不含凭证）。
type SourceDTO struct {
	ID           uint64    `json:"id"`
	Type         string    `json:"type"`
	Owner        string    `json:"owner"`
	Repo         string    `json:"repo"`
	Topic        string    `json:"topic"`
	WatchRelease bool      `json:"watch_release"`
	WatchIssues  bool      `json:"watch_issues"`
	WatchPulls   bool      `json:"watch_pulls"`
	WatchStars   bool      `json:"watch_stars"`
	WatchForks   bool      `json:"watch_forks"`
	UseWebhook   bool      `json:"use_webhook"`
	IntervalSec  int       `json:"interval_sec"`
	LastStars    int64     `json:"last_stars"`
	LastForks    int64     `json:"last_forks"`
	LastRunAt    *time.Time `json:"last_run_at"`
	LastError    string    `json:"last_error"`
	Enabled      bool      `json:"enabled"`
	WebhookReady bool      `json:"webhook_ready"`
	Mine         bool      `json:"mine"`
	Subscribed   bool      `json:"subscribed"`
	CreatedAt    time.Time `json:"created_at"`
}

// TokenDTO 令牌对外视图；Token 字段仅创建时返回一次。
type TokenDTO struct {
	ID              uint64 `json:"id"`
	Name            string `json:"name"`
	RateLimitPerMin int    `json:"rate_limit_per_min"`
	Token           string `json:"token,omitempty"`
	LastUsedAt      *time.Time `json:"last_used_at"`
	CreatedAt       time.Time `json:"created_at"`
}
