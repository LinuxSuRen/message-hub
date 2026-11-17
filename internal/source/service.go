package source

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/linuxsuren/message-hub/internal/model"
)

// Notifier 新消息产生后的推送回调（由 httpserver 注入，负责 SSE 分发）。
type Notifier func(msg *model.Message)

// Service 信息源核心服务：去重转存、初始同步与 star/fork 轮询。
type Service struct {
	db      *gorm.DB
	client  *GitHubClient
	key     []byte // 凭证加密主密钥（会话密钥同源）
	notify  Notifier
}

// NewService 创建服务。key 用于加密 GitHub PAT 与 webhook 密钥。
func NewService(db *gorm.DB, key []byte, apiURL string, notify Notifier) *Service {
	return &Service{db: db, client: NewGitHubClient(apiURL), key: key, notify: notify}
}

// Client 暴露 GitHub 客户端（建源校验、webhook 注册由 HTTP 层调用）。
func (s *Service) Client() *GitHubClient { return s.client }

// EncryptToken 加密凭证落库用。
func (s *Service) EncryptToken(plain string) (string, error) { return Encrypt(s.key, plain) }

// DecryptToken 解密凭证。
func (s *Service) DecryptToken(enc string) (string, error) { return Decrypt(s.key, enc) }

// Emit 去重转存一条事件：SourceItem 唯一索引挡住重复（webhook 重投、
// 初始同步与后续 webhook 的同一条事件），新消息落库后回调通知。
// 返回 false 表示重复条目被忽略。
func (s *Service) Emit(src *model.Source, item *EventItem) (bool, error) {
	item.clampMessage()
	msg := model.Message{
		TenantID: src.TenantID, UserID: src.UserID, SourceID: src.ID,
		Topic: src.Topic(), Title: item.Title, Body: item.Body,
		Level: item.Level, CreatedAt: time.Now(),
	}
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		row := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SourceItem{
			SourceID: src.ID, ExternalID: item.ExternalID, CreatedAt: msg.CreatedAt,
		})
		if row.Error != nil {
			return row.Error
		}
		if row.RowsAffected == 0 {
			return nil // 重复条目
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SourceItem{}).Where("source_id = ? AND external_id = ?", src.ID, item.ExternalID).
			Update("message_id", msg.ID).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if created && s.notify != nil {
		go s.notify(&msg)
	}
	return created, nil
}

// EmitRelease 初始同步：转存最新 release（与后续 webhook 事件共用 externalID 去重）。
func (s *Service) EmitRelease(src *model.Source, r *Release) (bool, error) {
	if r == nil {
		return false, nil
	}
	name := r.Name
	if name == "" {
		name = r.TagName
	}
	itemBody := r.URL
	if r.Body != "" {
		itemBody = truncateRunes(r.Body, 2000) + "\n" + r.URL
	}
	return s.Emit(src, &EventItem{
		ExternalID: fmt.Sprintf("release:published:%d", r.ID),
		Title:      "发布 " + name,
		Body:       itemBody,
		Level:      model.LevelInfo,
	})
}

// PollLoop 周期扫描到期源并轮询 star/fork；ctx 取消后退出。
func (s *Service) PollLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		s.PollDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PollDue 轮询全部到期源（每轮最多 20 个），返回本轮处理的源数。
func (s *Service) PollDue(ctx context.Context) int {
	var due []model.Source
	if err := s.db.Where("enabled = ? AND (watch_stars = ? OR watch_forks = ?) AND next_run_at <= ?",
		true, true, true, time.Now()).Order("next_run_at").Limit(20).Find(&due).Error; err != nil {
		log.Printf("消息源轮询查询失败: %v", err)
		return 0
	}
	for i := range due {
		if ctx.Err() != nil {
			return i
		}
		s.pollOne(ctx, &due[i])
	}
	return len(due)
}

// pollOne 拉取单个源的 star/fork 并对比基线；变化才产生消息，事务内同步更新基线。
func (s *Service) pollOne(ctx context.Context, src *model.Source) {
	token, err := s.DecryptToken(src.TokenEnc)
	if err != nil {
		s.markPollError(src, fmt.Sprintf("凭证解密失败，请重建消息源"))
		return
	}
	info, err := s.client.GetRepo(ctx, src.Owner, src.Repo, token)
	if err != nil {
		s.markPollError(src, truncateRunes(err.Error(), 500))
		return
	}
	now := time.Now()
	next := now.Add(time.Duration(src.IntervalSec) * time.Second)
	updates := map[string]any{
		"last_stars": info.Stars, "last_forks": info.Forks,
		"last_run_at": now, "next_run_at": next, "last_error": "",
	}
	var createdMsgs []model.Message
	err = s.db.Transaction(func(tx *gorm.DB) error {
		// 基线未知（-1，含新建与开关重开）只记录不发消息。
		if src.WatchStars && src.LastStars >= 0 && info.Stars != src.LastStars {
			if m, e := createPollMessage(tx, src, info.Stars, src.LastStars, "star"); e != nil {
				return e
			} else {
				createdMsgs = append(createdMsgs, m)
			}
		}
		if src.WatchForks && src.LastForks >= 0 && info.Forks != src.LastForks {
			if m, e := createPollMessage(tx, src, info.Forks, src.LastForks, "fork"); e != nil {
				return e
			} else {
				createdMsgs = append(createdMsgs, m)
			}
		}
		return tx.Model(&model.Source{}).Where("id = ?", src.ID).Updates(updates).Error
	})
	if err != nil {
		log.Printf("消息源 %s/%s 轮询落库失败: %v", src.Owner, src.Repo, err)
		return
	}
	if s.notify != nil {
		for i := range createdMsgs {
			go s.notify(&createdMsgs[i])
		}
	}
}

// createPollMessage 生成 star/fork 变化消息。
func createPollMessage(tx *gorm.DB, src *model.Source, current, previous int64, kind string) (model.Message, error) {
	delta := current - previous
	sign := "+"
	if delta < 0 {
		sign = ""
	}
	msg := model.Message{
		TenantID: src.TenantID, UserID: src.UserID, SourceID: src.ID,
		Topic: src.Topic(), Level: model.LevelInfo, CreatedAt: time.Now(),
		Title: fmt.Sprintf("%s %d → %d（%s%d）", kindLabel(kind), previous, current, sign, delta),
		Body:  fmt.Sprintf("https://github.com/%s/%s", src.Owner, src.Repo),
	}
	return msg, tx.Create(&msg).Error
}

func kindLabel(kind string) string {
	if kind == "fork" {
		return "fork 数"
	}
	return "star 数"
}

// markPollError 记录失败并退避；技术根因只入库与日志，不进用户文案。
func (s *Service) markPollError(src *model.Source, detail string) {
	log.Printf("消息源 %s/%s 轮询失败: %s", src.Owner, src.Repo, detail)
	backoff := time.Duration(src.IntervalSec) * time.Second
	if backoff < 10*time.Minute {
		backoff = 10 * time.Minute
	}
	s.db.Model(&model.Source{}).Where("id = ?", src.ID).Updates(map[string]any{
		"last_error": "拉取失败，稍后自动重试", "next_run_at": time.Now().Add(backoff),
	})
}
