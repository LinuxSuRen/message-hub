package httpserver

import (
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/linuxsuren/message-hub/internal/model"
)

// handleListMessages GET /api/v1/messages
//
// 两种翻页方式（二选一，page 优先）：
//   - 页码分页（Web 管理台）：?page=1&page_size=50 → 响应 {items,page,page_size,total}
//   - 游标分页（移动端兼容）：?limit=&cursor= → 响应 {items,next_cursor}
//
// 查询条件（与翻页方式正交）：scope=all|own|public、topic 精确匹配、
// q 关键字（标题/正文 LIKE）、level 级别。
// 可见范围：本人租户消息 + 公开消息 + 已订阅消息源的消息；scope 可进一步收窄。
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	query := r.URL.Query()

	// WHERE 构造：页码与游标两种模式共用
	where := func(q *gorm.DB) *gorm.DB {
		switch query.Get("scope") {
		case "own":
			q = q.Where("tenant_id = ?", user.TenantID)
		case "public":
			q = q.Where("public = ?", true)
		default: // all：租户 + 公开 + 我订阅的源
			subQ := s.db.Model(&model.SourceSub{}).Select("source_id").Where("user_id = ?", user.ID)
			q = q.Where("tenant_id = ? OR public = ? OR source_id IN (?)", user.TenantID, true, subQ)
		}
		if topic := strings.TrimSpace(query.Get("topic")); topic != "" {
			q = q.Where("topic = ?", topic)
		}
		if level := strings.TrimSpace(query.Get("level")); level != "" {
			q = q.Where("level = ?", level)
		}
		if kw := strings.TrimSpace(query.Get("q")); kw != "" {
			like := "%" + escapeLike(kw) + "%"
			q = q.Where("title LIKE ? ESCAPE '\\' OR body LIKE ? ESCAPE '\\'", like, like)
		}
		return q
	}

	if query.Get("page") != "" {
		page := clampInt(query.Get("page"), 1, 1, 1_000_000)
		pageSize := clampInt(query.Get("page_size"), 50, 1, 100)
		var total int64
		if err := where(s.db.Model(&model.Message{})).Count(&total).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "查询消息失败")
			return
		}
		var msgs []model.Message
		if err := where(s.db.Model(&model.Message{})).Order("id DESC").
			Limit(pageSize).Offset((page - 1) * pageSize).Find(&msgs).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "查询消息失败")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": msgs, "page": page, "page_size": pageSize, "total": total,
		})
		return
	}

	// 游标模式（旧契约，移动端在用）
	limit := clampInt(query.Get("limit"), 50, 1, 100)
	q := where(s.db.Model(&model.Message{})).Order("id DESC").Limit(limit + 1)
	if cursor := parseUint(query.Get("cursor")); cursor > 0 {
		q = q.Where("id < ?", cursor)
	}
	var msgs []model.Message
	if err := q.Find(&msgs).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "查询消息失败")
		return
	}
	var next any
	if len(msgs) > limit {
		msgs = msgs[:limit]
		next = msgs[limit-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": msgs, "next_cursor": next})
}

// escapeLike 转义 LIKE 通配符，避免关键字里的 %/_ 改变匹配语义。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// handleCreateMessage POST /api/v1/messages：推送消息（令牌认证时按令牌限流）。
func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Topic string `json:"topic"`
		Level string `json:"level"`
		Public bool  `json:"public"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Topic = strings.TrimSpace(req.Topic)
	if req.Title == "" || utf8.RuneCountInString(req.Title) > 128 {
		writeError(w, http.StatusBadRequest, "标题不能为空且不超过 128 字")
		return
	}
	if utf8.RuneCountInString(req.Body) > 4096 {
		writeError(w, http.StatusBadRequest, "正文不超过 4096 字")
		return
	}
	if utf8.RuneCountInString(req.Topic) > 64 {
		writeError(w, http.StatusBadRequest, "主题不超过 64 字")
		return
	}
	level := req.Level
	if level == "" {
		level = model.LevelInfo
	}
	valid := false
	for _, l := range model.Levels() {
		if level == l {
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "级别只支持 info、warning、error")
		return
	}
	user := s.currentUser(r)
	if req.Public && user.Role != model.RoleAdmin && user.Role != model.RoleTenantAdmin {
		writeError(w, http.StatusForbidden, "仅管理员角色可发送公开消息")
		return
	}
	if tokenID := s.currentTokenID(r); tokenID != 0 {
		// 配额来自 resolveAuth 已加载的令牌记录，避免二次查询的 fail-open 窗口。
		if !s.limiter.allow(tokenID, s.currentTokenRate(r)) {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "已触发流量限制，请降低推送频率")
			return
		}
	}
	msg := model.Message{
		TenantID: user.TenantID, UserID: user.ID,
		Topic: req.Topic, Title: req.Title, Body: req.Body,
		Level: level, Public: req.Public, CreatedAt: time.Now(),
	}
	if err := s.db.Create(&msg).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "消息保存失败")
		return
	}
	go s.publishMessage(&msg)
	writeJSON(w, http.StatusCreated, msg)
}
