package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestMessageListPagingAndQuery 页码分页与查询条件：
// page/page_size/total 契约、关键字、级别筛选，以及游标模式不受影响。
func TestMessageListPagingAndQuery(t *testing.T) {
	ts, _ := newTestServer(t)
	admin := newClient()
	registerUser(t, ts, admin, "admin@ex.com", "password123")

	// 造 5 条消息：3 条构建类（2 条 error）、2 条部署类
	push := func(title, body, level string) {
		status, _ := doJSON(t, admin, http.MethodPost, ts.URL+"/api/v1/messages",
			map[string]any{"title": title, "body": body, "level": level, "topic": "ci"}, nil)
		if status != http.StatusCreated {
			t.Fatalf("推送 %s 失败: %d", title, status)
		}
	}
	push("构建失败 pipeline-1", "compile error in main", "error")
	push("构建成功 pipeline-2", "all green", "info")
	push("部署完成 release-1", "deployed to prod", "info")
	push("部署告警 release-2", "latency high after deploy", "warning")
	push("构建失败 pipeline-3", "test timeout", "error")

	// 页码分页：page_size=2，共 5 条 3 页
	status, body := doJSON(t, admin, http.MethodGet, ts.URL+"/api/v1/messages?page=2&page_size=2", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("分页查询失败: %d", status)
	}
	if body["total"] != float64(5) || body["page"] != float64(2) || body["page_size"] != float64(2) {
		t.Fatalf("分页契约字段错误: %v", body)
	}
	if len(titlesOf(body)) != 2 {
		t.Fatalf("第 2 页应 2 条: %v", titlesOf(body))
	}
	// 第 3 页只剩 1 条
	_, body = doJSON(t, admin, http.MethodGet, ts.URL+"/api/v1/messages?page=3&page_size=2", nil, nil)
	if len(titlesOf(body)) != 1 {
		t.Fatalf("第 3 页应 1 条: %v", titlesOf(body))
	}
	// 超出范围返回空列表而非错误
	_, body = doJSON(t, admin, http.MethodGet, ts.URL+"/api/v1/messages?page=9&page_size=2", nil, nil)
	if len(titlesOf(body)) != 0 || body["total"] != float64(5) {
		t.Fatalf("越界页应为空但 total 仍为 5: %v", body)
	}

	// 关键字查询：标题命中 2 条
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+"/api/v1/messages?q="+url.QueryEscape("构建失败"), nil, nil)
	if len(titlesOf(body)) != 2 {
		t.Fatalf("标题关键字应命中 2 条: %v", titlesOf(body))
	}
	// 正文关键字
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+"/api/v1/messages?q="+url.QueryEscape("deploy"), nil, nil)
	if len(titlesOf(body)) != 2 {
		t.Fatalf("正文关键字应命中 2 条: %v", titlesOf(body))
	}
	// 关键字 + 级别组合：构建失败 且 error → 2；且 warning → 0
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+"/api/v1/messages?q="+url.QueryEscape("构建失败")+"&level=error", nil, nil)
	if len(titlesOf(body)) != 2 {
		t.Fatalf("组合查询应命中 2 条: %v", titlesOf(body))
	}
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+"/api/v1/messages?q="+url.QueryEscape("构建失败")+"&level=warning", nil, nil)
	if len(titlesOf(body)) != 0 {
		t.Fatalf("组合查询应命中 0 条: %v", titlesOf(body))
	}
	// LIKE 通配符被转义：含 % 的关键字按字面匹配
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+"/api/v1/messages?q="+url.QueryEscape("100%"), nil, nil)
	if len(titlesOf(body)) != 0 {
		t.Fatalf("通配符应按字面匹配: %v", titlesOf(body))
	}

	// 游标模式不受影响：limit=2 → 有 next_cursor，翻页正常
	_, body = doJSON(t, admin, http.MethodGet, ts.URL+"/api/v1/messages?limit=2", nil, nil)
	next, ok := body["next_cursor"].(float64)
	if !ok || next <= 0 {
		t.Fatalf("游标模式应返回 next_cursor: %v", body["next_cursor"])
	}
	_, body = doJSON(t, admin, http.MethodGet,
		ts.URL+fmt.Sprintf("/api/v1/messages?limit=2&cursor=%d", int(next)), nil, nil)
	if len(titlesOf(body)) != 2 {
		t.Fatalf("游标翻页应 2 条: %v", titlesOf(body))
	}
}
