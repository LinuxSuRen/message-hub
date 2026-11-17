package httpserver

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/linuxsuren/message-hub/internal/model"
	"github.com/linuxsuren/message-hub/internal/store"
)

// fakeGitHub 模拟 GitHub REST API：仓库元数据、release、webhook 增删。
type fakeGitHub struct {
	mu                sync.Mutex
	stars             int64
	hookID            int64
	hookEvents        []string
	hookURL           string
	hookSecret        string
	created           int
	deleted           int
	patched           int
	release           map[string]any
	unauthorizedToken string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{stars: 100, hookID: 0,
		release: map[string]any{"id": 111.0, "tag_name": "v0.1.0", "name": "v0.1.0",
			"html_url": "https://github.com/linuxsuren/message-hub/releases/tag/v0.1.0", "body": "首个版本"}}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.unauthorizedToken != "" && token == f.unauthorizedToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/linuxsuren/message-hub" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "linuxsuren/message-hub", "stargazers_count": f.stars, "forks_count": 10,
		})
	case r.URL.Path == "/repos/linuxsuren/message-hub/releases/latest" && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(f.release)
	case r.URL.Path == "/repos/linuxsuren/message-hub/hooks" && r.Method == http.MethodPost:
		var body struct {
			Events []string `json:"events"`
			Config struct {
				URL    string `json:"url"`
				Secret string `json:"secret"`
			} `json:"config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created++
		f.hookID = 555
		f.hookEvents = body.Events
		f.hookURL = body.Config.URL
		f.hookSecret = body.Config.Secret
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.hookID})
	case strings.HasPrefix(r.URL.Path, "/repos/linuxsuren/message-hub/hooks/") && r.Method == http.MethodPatch:
		f.patched++
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.hookID})
	case strings.HasPrefix(r.URL.Path, "/repos/linuxsuren/message-hub/hooks/") && r.Method == http.MethodDelete:
		f.deleted++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// newTestServerWithGitHub 构造带 fake GitHub 的测试服务。
func newTestServerWithGitHub(t *testing.T, fake *fakeGitHub, baseURL string) (*httptest.Server, *Server) {
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
	fakeSrv := httptest.NewServer(fake)
	t.Cleanup(fakeSrv.Close)
	s := New(db, Config{
		BaseURL:          baseURL,
		SessionSecret:    []byte("unit-test-secret"),
		RegistrationOpen: true,
		GitHubAPI:        fakeSrv.URL,
		Version:          "test",
	}, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func signPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postWebhook 投递 webhook 事件；不调用 testing API，可在 goroutine 中使用。
func postWebhook(hookID, event, secret, payload string, valid bool, target string) (int, map[string]any, error) {
	body := []byte(payload)
	headers := map[string]string{
		"Content-Type":        "application/json",
		"X-GitHub-Event":      event,
		"X-GitHub-Hook-ID":    hookID,
		"X-Hub-Signature-256": signPayload(secret, body),
	}
	if !valid {
		headers["X-Hub-Signature-256"] = "sha256=" + strings.Repeat("0", 64)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, target+"/api/v1/webhooks/github", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

// mustPostWebhook 同步投递并断言请求成功发出。
func mustPostWebhook(t *testing.T, ts *httptest.Server, hookID, event, secret, payload string, valid bool) (int, map[string]any) {
	t.Helper()
	status, body, err := postWebhook(hookID, event, secret, payload, valid, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

const releasePayload = `{"action":"published","release":{"id":111,"tag_name":"v0.1.0","name":"v0.1.0","html_url":"https://github.com/linuxsuren/message-hub/releases/tag/v0.1.0","body":"首个版本"},"repository":{"full_name":"linuxsuren/message-hub"}}`
const issuePayload = `{"action":"opened","issue":{"id":222,"number":42,"title":"登录异常","html_url":"https://github.com/linuxsuren/message-hub/issues/42","user":{"login":"octocat"}},"repository":{"full_name":"linuxsuren/message-hub"}}`

// TestSourceLifecycleAndSubscription 完整链路：建源自动注册 webhook、
// 初始同步、验签接收、去重、订阅可见性、权限与删除清理。
func TestSourceLifecycleAndSubscription(t *testing.T) {
	fake := newFakeGitHub()
	ts, s := newTestServerWithGitHub(t, fake, "http://hub.test")

	// 首用户（平台管理员）建源
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_valid",
		"watch_release": true, "watch_issues": true, "watch_pulls": true,
		"watch_stars": true, "watch_forks": true, "use_webhook": true, "interval_sec": 60,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建源失败: %d %v", status, body)
	}
	if body["topic"] != "github/linuxsuren/message-hub" || body["webhook_ready"] != true || body["subscribed"] != true {
		t.Fatalf("建源响应字段异常: %v", body)
	}

	// fake 应收到 webhook 注册：事件齐全、回调地址与密钥正确
	fake.mu.Lock()
	created, events, hookURL, secret := fake.created, append([]string(nil), fake.hookEvents...), fake.hookURL, fake.hookSecret
	fake.mu.Unlock()
	if created != 1 {
		t.Fatalf("应注册 1 个 webhook: %d", created)
	}
	if len(events) != 3 || events[0] != "release" || events[1] != "issues" || events[2] != "pull_request" {
		t.Fatalf("webhook 事件错误: %v", events)
	}
	if hookURL != "http://hub.test/api/v1/webhooks/github" {
		t.Fatalf("webhook 回调地址错误: %s", hookURL)
	}
	if secret == "" {
		t.Fatal("webhook 密钥不应为空")
	}

	// 初始同步：最新 release 应已转存为消息
	_, body = doJSON(t, admin, http.MethodGet, ts.URL+"/api/v1/messages?scope=all", nil, nil)
	if !contains(titlesOf(body), "发布 v0.1.0") {
		t.Fatalf("初始同步应转存最新 release: %v", titlesOf(body))
	}

	// webhook 重投同一 release（同 externalID）应被去重
	status, body = mustPostWebhook(t, ts, "555", "release", secret, releasePayload, true)
	if status != http.StatusOK || body["created"] != false {
		t.Fatalf("重复 release 应去重: %d %v", status, body)
	}

	// 新 issue 事件正常转存
	status, body = mustPostWebhook(t, ts, "555", "issues", secret, issuePayload, true)
	if status != http.StatusOK || body["created"] != true {
		t.Fatalf("issue 事件应转存: %d %v", status, body)
	}

	// 坏签名拒绝
	if status, _ = mustPostWebhook(t, ts, "555", "issues", secret, issuePayload, false); status != http.StatusUnauthorized {
		t.Fatalf("坏签名应 401: %d", status)
	}

	// 其它租户未订阅用户看不到源消息；订阅后可见
	_, tenant := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/tenants", map[string]string{"name": "factory"}, nil)
	tenantID := int(tenant["id"].(float64))
	doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/users",
		map[string]any{"email": "worker@ex.com", "password": "password123", "tenant_id": tenantID}, nil)
	worker := newClient()
	loginUser(t, ts, worker, "worker@ex.com", "password123")
	_, body = doJSON(t, worker, http.MethodGet, ts.URL+"/api/v1/messages?scope=all", nil, nil)
	if contains(titlesOf(body), "发布 v0.1.0") {
		t.Fatal("未订阅用户不应看到源消息")
	}
	if status, _ = doJSON(t, worker, http.MethodPost, ts.URL+"/api/v1/sources/1/subscription", nil, nil); status != http.StatusNoContent {
		t.Fatalf("订阅失败: %d", status)
	}
	_, body = doJSON(t, worker, http.MethodGet, ts.URL+"/api/v1/messages?scope=all", nil, nil)
	if !contains(titlesOf(body), "发布 v0.1.0") {
		t.Fatalf("订阅后应看到源消息: %v", titlesOf(body))
	}
	// topic 过滤
	_, body = doJSON(t, worker, http.MethodGet, ts.URL+"/api/v1/messages?topic=github/linuxsuren/message-hub", nil, nil)
	if len(titlesOf(body)) == 0 {
		t.Fatal("按主题过滤应有结果")
	}

	// 重复建源 409
	if status, _ = doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources",
		map[string]any{"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_x", "watch_stars": true}, nil); status != http.StatusConflict {
		t.Fatalf("重复建源应 409: %d", status)
	}

	// 非创建者不能改/删
	if status, _ = doJSON(t, worker, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"enabled": false}, nil); status != http.StatusForbidden {
		t.Fatalf("非创建者修改应 403: %d", status)
	}
	if status, _ = doJSON(t, worker, http.MethodDelete, ts.URL+"/api/v1/sources/1", nil, nil); status != http.StatusForbidden {
		t.Fatalf("非创建者删除应 403: %d", status)
	}

	// 创建者关闭 issues 关注 → PATCH webhook 事件；关闭同步 → 删除 webhook
	if status, _ = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"watch_issues": false}, nil); status != http.StatusOK {
		t.Fatalf("更新失败: %d", status)
	}
	fake.mu.Lock()
	patched := fake.patched
	fake.mu.Unlock()
	if patched != 1 {
		t.Fatalf("应同步更新 webhook 事件: %d", patched)
	}
	// 关闭后 issue 事件被忽略
	status, body = mustPostWebhook(t, ts, "555", "issues", secret, issuePayload, true)
	if status != http.StatusOK || body["status"] != "ignored" {
		t.Fatalf("关闭关注后应忽略: %d %v", status, body)
	}

	// 用户关闭 webhook 同步 → GitHub 侧删除
	if status, body = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"use_webhook": false}, nil); status != http.StatusOK || body["webhook_ready"] != false {
		t.Fatalf("关闭同步应清理 webhook: %d %v", status, body)
	}
	fake.mu.Lock()
	deletedOnce := fake.deleted
	fake.mu.Unlock()
	if deletedOnce != 1 {
		t.Fatalf("关闭同步应删除 webhook: %d", deletedOnce)
	}

	// 停用源后所有事件被忽略；重新启用恢复
	doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1", map[string]any{"enabled": false}, nil)
	if status, body = mustPostWebhook(t, ts, "555", "release", secret, releasePayload, true); status != http.StatusOK || body["status"] != "ignored" {
		t.Fatalf("停用后应忽略事件: %d %v", status, body)
	}
	doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1", map[string]any{"enabled": true}, nil)

	// 删除源：清理 webhook 与订阅
	if status, _ = doJSON(t, admin, http.MethodDelete, ts.URL+"/api/v1/sources/1", nil, nil); status != http.StatusNoContent {
		t.Fatalf("删除失败: %d", status)
	}
	fake.mu.Lock()
	deleted := fake.deleted
	fake.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("删除源应清理 webhook: %d", deleted)
	}
	var subCount int64
	s.db.Model(&model.SourceSub{}).Count(&subCount)
	if subCount != 0 {
		t.Fatalf("订阅应随源清除: %d", subCount)
	}
}

// TestSourceCreateLocalhostBaseURLRejected 回调地址不可达公网时应拒绝建源。
func TestSourceCreateLocalhostBaseURLRejected(t *testing.T) {
	fake := newFakeGitHub()
	ts, _ := newTestServerWithGitHub(t, fake, "http://localhost:8080")
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_valid",
		"watch_release": true, "use_webhook": true,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("localhost 回调应拒绝: %d %v", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "公网") {
		t.Fatalf("错误提示应指向公网地址配置: %v", body)
	}
}

// TestSourceCreateBadToken 无效令牌映射为可读错误。
func TestSourceCreateBadToken(t *testing.T) {
	fake := newFakeGitHub()
	fake.unauthorizedToken = "ghp_bad"
	ts, _ := newTestServerWithGitHub(t, fake, "http://hub.test")
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_bad", "watch_stars": true,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("无效令牌应 400: %d %v", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "令牌") {
		t.Fatalf("错误提示应包含令牌指引: %v", body)
	}
}

// TestSourceCreateDefaultWatchesFalse 未勾选的关注项必须保持 false，
// 回归：gorm default:true 曾把未勾选的 watch_release 静默置 true。
func TestSourceCreateDefaultWatchesFalse(t *testing.T) {
	fake := newFakeGitHub()
	ts, _ := newTestServerWithGitHub(t, fake, "http://hub.test")
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_valid", "watch_stars": true,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建源失败: %d %v", status, body)
	}
	for _, key := range []string{"watch_release", "watch_issues", "watch_pulls", "watch_forks", "use_webhook"} {
		if body[key] == true {
			t.Fatalf("未勾选的 %s 不应为 true: %v", key, body)
		}
	}
}

// TestSourceUpdateToken 更换令牌：新令牌验证通过后加密落库；
// 仅改令牌不应触发多余的 webhook PATCH；无效令牌被拒绝且不影响已存令牌。
func TestSourceUpdateToken(t *testing.T) {
	fake := newFakeGitHub()
	ts, s := newTestServerWithGitHub(t, fake, "http://hub.test")
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_old",
		"watch_release": true, "watch_stars": true,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建源失败: %d %v", status, body)
	}

	// 仅更换令牌
	status, body = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"token": "ghp_new"}, nil)
	if status != http.StatusOK {
		t.Fatalf("更换令牌失败: %d %v", status, body)
	}
	var src model.Source
	s.db.First(&src, 1)
	if got, err := s.sources.DecryptToken(src.TokenEnc); err != nil || got != "ghp_new" {
		t.Fatalf("落库应为新令牌: %q %v", got, err)
	}
	fake.mu.Lock()
	patched := fake.patched
	fake.mu.Unlock()
	if patched != 0 {
		t.Fatalf("仅改令牌不应触发 webhook PATCH: %d", patched)
	}

	// 无效令牌拒绝，已存令牌保持不变
	fake.unauthorizedToken = "ghp_bad"
	status, body = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"token": "ghp_bad"}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("无效新令牌应 400: %d %v", status, body)
	}
	s.db.First(&src, 1)
	if got, _ := s.sources.DecryptToken(src.TokenEnc); got != "ghp_new" {
		t.Fatalf("失败的新令牌不应覆盖旧值: %q", got)
	}
}

// TestSettingsAndOptionalWebhook 系统设置（对外地址）+ webhook 可选：
// 不开 use_webhook 时无公网地址也能建源；开了才注册，地址取系统设置；
// 设置接口仅平台管理员可写。
func TestSettingsAndOptionalWebhook(t *testing.T) {
	fake := newFakeGitHub()
	ts, _ := newTestServerWithGitHub(t, fake, "http://localhost:8080") // 本机地址，无公网
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")

	// 1. 不开 webhook：本机地址也允许建源（此前会被拒绝）
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_valid",
		"watch_release": true, "watch_stars": true,
	}, nil)
	if status != http.StatusCreated || body["webhook_ready"] != false || body["use_webhook"] != false {
		t.Fatalf("不开 webhook 应建源成功: %d %v", status, body)
	}
	fake.mu.Lock()
	created := fake.created
	fake.mu.Unlock()
	if created != 0 {
		t.Fatalf("未开启同步不应注册 webhook: %d", created)
	}

	// 2. 先在未配置地址时尝试开启同步（该源勾了 watch_release）→ 应拒绝
	if status, body = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"use_webhook": true}, nil); status != http.StatusBadRequest {
		t.Fatalf("无公网地址开启同步应 400: %d %v", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "系统设置") {
		t.Fatalf("错误提示应指向系统设置: %v", body)
	}

	// 3. 设置接口权限与校验
	plain := newClient()
	_, tenant := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/tenants", map[string]string{"name": "team"}, nil)
	doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/users",
		map[string]any{"email": "plain@ex.com", "password": "password123", "tenant_id": int(tenant["id"].(float64))}, nil)
	loginUser(t, ts, plain, "plain@ex.com", "password123")
	if status, _ = doJSON(t, plain, http.MethodPut, ts.URL+"/api/v1/settings",
		map[string]string{"base_url": "https://x.example.com"}, nil); status != http.StatusForbidden {
		t.Fatalf("非管理员改设置应 403: %d", status)
	}
	if status, _ = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/settings",
		map[string]string{"base_url": "not-a-url"}, nil); status != http.StatusBadRequest {
		t.Fatalf("非法地址应 400: %d", status)
	}
	if status, _ = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/settings",
		map[string]string{"base_url": "https://msg.example.com"}, nil); status != http.StatusOK {
		t.Fatalf("保存设置失败: %d", status)
	}
	_, body = doJSON(t, plain, http.MethodGet, ts.URL+"/api/v1/settings", nil, nil) // 登录即可读
	if body["base_url"] != "https://msg.example.com" {
		t.Fatalf("设置读取错误: %v", body)
	}

	// 4. 配置地址后开启同步：用系统设置的公网地址注册 webhook
	status, body = doJSON(t, admin, http.MethodPut, ts.URL+"/api/v1/sources/1",
		map[string]any{"use_webhook": true}, nil)
	if status != http.StatusOK || body["webhook_ready"] != true {
		t.Fatalf("开启同步应注册 webhook: %d %v", status, body)
	}
	fake.mu.Lock()
	hookCount := fake.created
	callbackURL := fake.hookURL
	fake.mu.Unlock()
	if hookCount != 1 || callbackURL != "https://msg.example.com/api/v1/webhooks/github" {
		t.Fatalf("webhook 应使用系统设置地址: %d %s", hookCount, callbackURL)
	}
}

// TestSSESourceDelivery SSE 只推给订阅者（跨租户按用户定向）。
func TestSSESourceDelivery(t *testing.T) {
	fake := newFakeGitHub()
	ts, s := newTestServerWithGitHub(t, fake, "http://hub.test")

	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")
	status, body := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/sources", map[string]any{
		"owner": "linuxsuren", "repo": "message-hub", "token": "ghp_valid",
		"watch_issues": true, "use_webhook": true,
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("建源失败: %d %v", status, body)
	}

	// 取验签密钥
	var src model.Source
	s.db.First(&src)
	secret, err := s.sources.DecryptToken(src.SecretEnc)
	if err != nil || secret == "" {
		t.Fatalf("验签密钥读取失败: %v", err)
	}

	// 其它租户用户订阅源后经 SSE 实时收到
	_, tenant := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/tenants", map[string]string{"name": "factory"}, nil)
	tenantID := int(tenant["id"].(float64))
	doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/users",
		map[string]any{"email": "worker@ex.com", "password": "password123", "tenant_id": tenantID}, nil)
	worker := newClient()
	loginUser(t, ts, worker, "worker@ex.com", "password123")
	workerToken := createToken(t, ts, worker, 1000)
	doJSON(t, worker, http.MethodPost, ts.URL+"/api/v1/sources/1/subscription", nil, nil)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/stream", nil)
	req.Header.Set("Authorization", "Bearer "+workerToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 64)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- line
		}
	}()
	readLine := func() string {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("SSE 提前关闭")
			}
			return line
		case <-time.After(5 * time.Second):
			t.Fatal("等待 SSE 帧超时")
			return ""
		}
	}
	readLine() // retry
	readLine() // 空行

	go func() {
		_, _, _ = postWebhook(fmt.Sprint(src.WebhookID), "issues", secret, issuePayload, true, ts.URL)
	}()

	deadline := time.After(5 * time.Second)
	for {
		line := readLine()
		if strings.Contains(line, `"title":`) {
			if strings.Contains(line, "登录异常") {
				break
			}
			t.Fatalf("应收到订阅源消息: %s", line)
		}
		select {
		case <-deadline:
			t.Fatal("未在期限内收到源消息")
		default:
		}
	}
}
