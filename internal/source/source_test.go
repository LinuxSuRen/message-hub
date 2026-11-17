package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/linuxsuren/message-hub/internal/model"
	"github.com/linuxsuren/message-hub/internal/store"
)

// openTestDB 打开临时库并注册清理（Windows 下必须显式关闭才能删除文件）。
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, e := db.DB(); e == nil {
			sqlDB.Close()
		}
	})
	return db
}

func TestEncryptDecryptRoundtrip(t *testing.T) {
	key := []byte("unit-test-secret")
	enc, err := Encrypt(key, "ghp_secret-token-123")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if strings.Contains(enc, "ghp_secret-token-123") {
		t.Fatal("密文不得包含明文")
	}
	plain, err := Decrypt(key, enc)
	if err != nil || plain != "ghp_secret-token-123" {
		t.Fatalf("解密结果错误: %q %v", plain, err)
	}
	// 换密钥解密必须失败
	if _, err := Decrypt([]byte("another-key-0123456789"), enc); err == nil {
		t.Fatal("错误密钥不应解出明文")
	}
}

func TestParseEvents(t *testing.T) {
	release := `{"action":"published","release":{"id":111,"tag_name":"v1.0.0","name":"v1.0.0","html_url":"https://github.com/o/r/releases/tag/v1.0.0","body":"notes"},"repository":{"full_name":"o/r"}}`
	item := ParseEvent("release", []byte(release))
	if item == nil || item.ExternalID != "release:published:111" {
		t.Fatalf("release 事件解析错误: %+v", item)
	}
	if !strings.Contains(item.Title, "v1.0.0") {
		t.Fatalf("标题应包含版本号: %q", item.Title)
	}
	// 非 published 忽略
	if item = ParseEvent("release", []byte(`{"action":"edited","release":{"id":111}}`)); item != nil {
		t.Fatal("edited release 应忽略")
	}

	issue := `{"action":"opened","issue":{"id":222,"number":42,"title":"bug","html_url":"https://github.com/o/r/issues/42","user":{"login":"octo"}},"repository":{"full_name":"o/r"}}`
	item = ParseEvent("issues", []byte(issue))
	if item == nil || item.ExternalID != "issues:opened:222" || !strings.Contains(item.Title, "#42") {
		t.Fatalf("issue 事件解析错误: %+v", item)
	}

	pr := `{"action":"closed","pull_request":{"id":333,"number":7,"title":"fix","html_url":"https://github.com/o/r/pull/7","merged":true},"repository":{"full_name":"o/r"}}`
	item = ParseEvent("pull_request", []byte(pr))
	if item == nil || item.ExternalID != "pull:merged:333" || !strings.Contains(item.Title, "PR 已合并") {
		t.Fatalf("PR 合并事件解析错误: %+v", item)
	}

	if ParseEvent("ping", []byte(`{}`)) != nil {
		t.Fatal("ping 应忽略")
	}
	if ParseEvent("push", []byte(`{}`)) != nil {
		t.Fatal("未知事件应忽略")
	}
	if got := ParseRepoFullName([]byte(release)); got != "o/r" {
		t.Fatalf("仓库全名解析错误: %q", got)
	}
}

func TestEmitDedup(t *testing.T) {
	db := openTestDB(t)
	var notified int64
	svc := NewService(db, []byte("test-key"), "", func(*model.Message) { atomic.AddInt64(&notified, 1) })
	src := &model.Source{ID: 1, TenantID: 1, UserID: 1, Type: "github", Owner: "o", Repo: "r"}
	item := &EventItem{ExternalID: "release:published:1", Title: "发布 v1", Level: model.LevelInfo}

	created, err := svc.Emit(src, item)
	if err != nil || !created {
		t.Fatalf("首次转存应成功: %v %v", created, err)
	}
	created, err = svc.Emit(src, item)
	if err != nil || created {
		t.Fatalf("重复条目应被忽略: %v %v", created, err)
	}
	var msgCount, itemCount int64
	db.Model(&model.Message{}).Count(&msgCount)
	db.Model(&model.SourceItem{}).Count(&itemCount)
	if msgCount != 1 || itemCount != 1 {
		t.Fatalf("去重失效: messages=%d items=%d", msgCount, itemCount)
	}
}

// TestPollDueStarChange 轮询发现 star 变化生成一条消息；基线未知时只记录不告警。
func TestPollDueStarChange(t *testing.T) {
	var stars atomic.Int64
	stars.Store(100)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "o/r", "stargazers_count": stars.Load(), "forks_count": 10,
		})
	}))
	t.Cleanup(fake.Close)

	db := openTestDB(t)
	var notified int64
	svc := NewService(db, []byte("test-key"), fake.URL, func(*model.Message) { atomic.AddInt64(&notified, 1) })
	tokenEnc, _ := Encrypt([]byte("test-key"), "ghp_x")
	past := time.Now().Add(-time.Minute)
	src := model.Source{
		ID: 1, TenantID: 1, UserID: 1, Type: "github", Owner: "o", Repo: "r",
		TokenEnc: tokenEnc, WatchStars: true, IntervalSec: 60, Enabled: true,
		LastStars: -1, LastForks: 10, NextRunAt: past,
	}
	if err := db.Create(&src).Error; err != nil {
		t.Fatal(err)
	}

	// 基线未知：只建立基线，不发消息
	if n := svc.PollDue(context.Background()); n != 1 {
		t.Fatalf("应处理 1 个到期源: %d", n)
	}
	var after model.Source
	db.First(&after, 1)
	if after.LastStars != 100 {
		t.Fatalf("基线应为 100: %d", after.LastStars)
	}
	var msgCount int64
	db.Model(&model.Message{}).Count(&msgCount)
	if msgCount != 0 {
		t.Fatalf("建立基线不应产生消息: %d", msgCount)
	}

	// star 变化：产生一条消息并推送
	stars.Store(105)
	db.Model(&model.Source{}).Where("id = ?", 1).Update("next_run_at", past)
	svc.PollDue(context.Background())
	var msg model.Message
	if err := db.Where("source_id = ?", 1).First(&msg).Error; err != nil {
		t.Fatalf("应产生 star 变化消息: %v", err)
	}
	if !strings.Contains(msg.Title, "100 → 105") || !strings.Contains(msg.Title, "+5") {
		t.Fatalf("消息标题应包含变化: %q", msg.Title)
	}
	db.First(&after, 1)
	if after.LastStars != 105 || after.LastError != "" {
		t.Fatalf("基线应更新为 105 且无错误: %+v", after)
	}
	if atomic.LoadInt64(&notified) != 1 {
		t.Fatalf("应通知一次: %d", notified)
	}

	// 无变化：不再产生消息
	db.Model(&model.Source{}).Where("id = ?", 1).Update("next_run_at", past)
	svc.PollDue(context.Background())
	db.Model(&model.Message{}).Count(&msgCount)
	if msgCount != 1 {
		t.Fatalf("无变化不应重复产生消息: %d", msgCount)
	}
}
