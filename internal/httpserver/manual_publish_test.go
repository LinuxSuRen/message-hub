package httpserver

import (
	"net/http"
	"testing"
)

// TestManualPublishViaSession Web 管理台手动发布消息的后端路径：
// 会话登录即可推送；公开消息仅管理员角色；未登录拒绝。
func TestManualPublishViaSession(t *testing.T) {
	ts, mailer := newTestServer(t)
	admin := newClient()
	if status, _ := registerUser(t, ts, admin, "admin@ex.com", "password123"); status != http.StatusCreated {
		t.Fatal("初始化管理员失败")
	}

	// 未登录 401
	if status, _ := doJSON(t, newClient(), http.MethodPost, ts.URL+"/api/v1/messages",
		map[string]any{"title": "x"}, nil); status != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d", status)
	}

	// 管理员会话发布公开消息
	if status, _ := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/messages",
		map[string]any{"title": "公告", "body": "今晚维护", "level": "warning", "topic": "ops", "public": true},
		nil); status != http.StatusCreated {
		t.Fatalf("管理员发公开消息应 201")
	}

	// 普通用户（会话）发布私有消息成功；公开消息 403
	doJSON(t, newClient(), http.MethodPost, ts.URL+"/api/v1/auth/verification-code",
		map[string]string{"email": "plain@ex.com"}, nil)
	plain := newClient()
	doJSON(t, plain, http.MethodPost, ts.URL+"/api/v1/auth/register",
		map[string]string{"email": "plain@ex.com", "password": "password123", "code": mailer.code("plain@ex.com")}, nil)
	if status, _ := doJSON(t, plain, http.MethodPost, ts.URL+"/api/v1/messages",
		map[string]any{"title": "我的消息"}, nil); status != http.StatusCreated {
		t.Fatalf("普通用户发私有消息应 201: %d", status)
	}
	if status, _ := doJSON(t, plain, http.MethodPost, ts.URL+"/api/v1/messages",
		map[string]any{"title": "越权", "public": true}, nil); status != http.StatusForbidden {
		t.Fatalf("普通用户发公开消息应 403: %d", status)
	}

	// 双方都能在列表看到各自可见的消息
	_, body := doJSON(t, plain, http.MethodGet, ts.URL+"/api/v1/messages?scope=all", nil, nil)
	titles := titlesOf(body)
	if !contains(titles, "公告") || !contains(titles, "我的消息") || contains(titles, "越权") {
		t.Fatalf("普通用户可见范围错误: %v", titles)
	}
}
