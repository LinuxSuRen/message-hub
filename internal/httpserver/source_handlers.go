package httpserver

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"github.com/linuxsuren/message-hub/internal/model"
	"github.com/linuxsuren/message-hub/internal/source"
)

// ownerRepoRe GitHub owner/repo 合法字符。
var ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// handleListSources GET /api/v1/sources：全部消息源（供浏览与订阅），附带本人订阅态。
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	var srcs []model.Source
	if err := s.db.Order("id DESC").Find(&srcs).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "查询消息源失败")
		return
	}
	var subs []model.SourceSub
	s.db.Where("user_id = ?", user.ID).Find(&subs)
	mine := map[uint64]bool{}
	for _, sub := range subs {
		mine[sub.SourceID] = true
	}
	items := make([]model.SourceDTO, 0, len(srcs))
	for i := range srcs {
		items = append(items, toSourceDTO(&srcs[i], user, mine[srcs[i].ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleCreateSource POST /api/v1/sources：创建 GitHub 仓库监视源。
// 提交的 PAT 仅创建时出现，验证 + 注册 webhook 后加密落库，不再回显。
func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	user := s.currentUser(r)
	var req struct {
		Owner        string `json:"owner"`
		Repo         string `json:"repo"`
		Token        string `json:"token"`
		WatchRelease bool   `json:"watch_release"`
		WatchIssues  bool   `json:"watch_issues"`
		WatchPulls   bool   `json:"watch_pulls"`
		WatchStars   bool   `json:"watch_stars"`
		WatchForks   bool   `json:"watch_forks"`
		UseWebhook   bool   `json:"use_webhook"`
		IntervalSec  int    `json:"interval_sec"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	req.Owner, req.Repo = trim(req.Owner), trim(req.Repo)
	if !ownerRepoRe.MatchString(req.Owner) || !ownerRepoRe.MatchString(req.Repo) {
		writeError(w, http.StatusBadRequest, "owner 或 repo 格式不合法")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "请填写 GitHub 访问令牌")
		return
	}
	if !req.WatchRelease && !req.WatchIssues && !req.WatchPulls && !req.WatchStars && !req.WatchForks {
		writeError(w, http.StatusBadRequest, "请至少选择一项关注内容")
		return
	}
	if req.IntervalSec <= 0 {
		req.IntervalSec = 300
	}
	if req.IntervalSec < 60 {
		req.IntervalSec = 60
	}
	if req.IntervalSec > 86400 {
		req.IntervalSec = 86400
	}

	var count int64
	s.db.Model(&model.Source{}).Where("type = ? AND owner = ? AND repo = ?", "github", req.Owner, req.Repo).Count(&count)
	if count > 0 {
		writeError(w, http.StatusConflict, "该仓库已存在消息源，可直接订阅")
		return
	}

	ctx, cancel := contextWithTimeout(r)
	defer cancel()
	client := s.sources.Client()

	// 1. 校验令牌与仓库，同时取 star/fork 基线。
	info, err := client.GetRepo(ctx, req.Owner, req.Repo, req.Token)
	if err != nil {
		writeMappedError(w, err, "校验仓库信息失败")
		return
	}

	src := model.Source{
		TenantID: user.TenantID, UserID: user.ID,
		Type: "github", Owner: req.Owner, Repo: req.Repo,
		WatchRelease: req.WatchRelease, WatchIssues: req.WatchIssues,
		WatchPulls: req.WatchPulls, WatchStars: req.WatchStars, WatchForks: req.WatchForks,
		UseWebhook: req.UseWebhook,
		IntervalSec: req.IntervalSec,
		LastStars:   info.Stars, LastForks: info.Forks,
		Enabled:     true, CreatedAt: time.Now(),
	}
	if src.TokenEnc, err = s.sources.EncryptToken(req.Token); err != nil {
		writeError(w, http.StatusInternalServerError, "凭证保存失败")
		return
	}

	// 2. 用户开启 webhook 同步时才注册（需要对外可达地址）；未开启只做轮询。
	if src.WantWebhook() {
		if problem := s.checkWebhookBaseURL(); problem != "" {
			writeError(w, http.StatusBadRequest, problem)
			return
		}
		secret := randomHex(32)
		hookID, err := client.CreateWebhook(ctx, req.Owner, req.Repo, req.Token,
			s.webhookCallbackURL(), secret, src.WebhookEvents())
		if err != nil {
			writeMappedError(w, err, "注册 webhook 失败，请检查令牌是否具备仓库管理权限")
			return
		}
		src.WebhookID = hookID
		if src.SecretEnc, err = s.sources.EncryptToken(secret); err != nil {
			writeError(w, http.StatusInternalServerError, "凭证保存失败")
			return
		}
	}

	// 3. 落库并自动订阅创建者。
	if err := s.db.Create(&src).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "消息源保存失败")
		return
	}
	s.db.Clauses(onConflictDoNothing()).Create(&model.SourceSub{SourceID: src.ID, UserID: user.ID, CreatedAt: time.Now()})

	// 4. 初始同步：转存最新 release，首条消息立即可见。
	if src.WatchRelease {
		if rel, err := client.LatestRelease(ctx, req.Owner, req.Repo, req.Token); err == nil && rel != nil {
			if _, err := s.sources.EmitRelease(&src, rel); err != nil {
				log.Printf("消息源 %s 初始同步 release 失败: %v", src.Topic(), err)
			}
		}
	}

	log.Printf("消息源创建：%s（webhook=%d，创建者 uid=%d）", src.Topic(), src.WebhookID, user.ID)
	writeJSON(w, http.StatusCreated, toSourceDTO(&src, user, true))
}

// handleUpdateSource PUT /api/v1/sources/{id}：调整启停、关注内容、轮询间隔，
// 以及更换 GitHub 令牌（token 非空时先验证再加密落库，轮询器下轮生效）。
// owner/repo 是源的身份字段，不支持修改——换仓库请删除后重建。
func (s *Server) handleUpdateSource(w http.ResponseWriter, r *http.Request) {
	src := s.findSource(w, r)
	if src == nil {
		return
	}
	user := s.currentUser(r)
	if user.ID != src.UserID && user.Role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "只有消息源创建者或平台管理员可以修改")
		return
	}
	var req struct {
		Enabled      *bool  `json:"enabled"`
		WatchRelease *bool  `json:"watch_release"`
		WatchIssues  *bool  `json:"watch_issues"`
		WatchPulls   *bool  `json:"watch_pulls"`
		WatchStars   *bool  `json:"watch_stars"`
		WatchForks   *bool  `json:"watch_forks"`
		UseWebhook   *bool  `json:"use_webhook"`
		IntervalSec  *int   `json:"interval_sec"`
		Token        string `json:"token"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	before := *src
	if req.Enabled != nil {
		src.Enabled = *req.Enabled
	}
	apply := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	apply(&src.WatchRelease, req.WatchRelease)
	apply(&src.WatchIssues, req.WatchIssues)
	apply(&src.WatchPulls, req.WatchPulls)
	apply(&src.WatchStars, req.WatchStars)
	apply(&src.WatchForks, req.WatchForks)
	apply(&src.UseWebhook, req.UseWebhook)
	if req.IntervalSec != nil {
		if *req.IntervalSec < 60 {
			*req.IntervalSec = 60
		}
		if *req.IntervalSec > 86400 {
			*req.IntervalSec = 86400
		}
		src.IntervalSec = *req.IntervalSec
	}
	// 新开启 star/fork 或重新启用：基线作废，下轮只记录不告警。
	if (!before.WatchStars && src.WatchStars) || (!before.Enabled && src.Enabled) {
		src.LastStars = -1
	}
	if (!before.WatchForks && src.WatchForks) || (!before.Enabled && src.Enabled) {
		src.LastForks = -1
	}
	if src.Enabled && (src.WatchStars || src.WatchForks) {
		src.NextRunAt = time.Now()
	}

	// 更换令牌：先用新令牌访问仓库验证可用，再加密落库。
	if req.Token != "" {
		ctx, cancel := contextWithTimeout(r)
		if _, err := s.sources.Client().GetRepo(ctx, src.Owner, src.Repo, req.Token); err != nil {
			cancel()
			writeMappedError(w, err, "新令牌校验失败")
			return
		}
		cancel()
		enc, err := s.sources.EncryptToken(req.Token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "凭证保存失败")
			return
		}
		src.TokenEnc = enc
	}

	// webhook 仅在必要时同步：需要且原来没有（新建）、开启且事件集合变化、
	// 不再需要（用户关闭同步或关掉全部 webhook 类关注）时删除。
	wantHookNow := src.WantWebhook()
	hadHook := before.WebhookID != 0
	eventsChanged := before.WantWebhook() && wantHookNow &&
		!equalStringSlices(before.WebhookEvents(), src.WebhookEvents())
	if (wantHookNow && !hadHook) || (wantHookNow && hadHook && eventsChanged) || (!wantHookNow && hadHook) {
		ctx, cancel := contextWithTimeout(r)
		defer cancel()
		token, err := s.sources.DecryptToken(src.TokenEnc)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "凭证读取失败，请更换令牌")
			return
		}
		switch {
		case wantHookNow && !hadHook:
			if problem := s.checkWebhookBaseURL(); problem != "" {
				writeError(w, http.StatusBadRequest, problem)
				return
			}
			secret := randomHex(32)
			hookID, err := s.sources.Client().CreateWebhook(ctx, src.Owner, src.Repo, token,
				s.webhookCallbackURL(), secret, src.WebhookEvents())
			if err != nil {
				writeMappedError(w, err, "注册 webhook 失败，请检查令牌是否具备仓库管理权限")
				return
			}
			src.WebhookID = hookID
			if src.SecretEnc, err = s.sources.EncryptToken(secret); err != nil {
				writeError(w, http.StatusInternalServerError, "凭证保存失败")
				return
			}
		case wantHookNow && hadHook:
			if err := s.sources.Client().UpdateWebhookEvents(ctx, src.Owner, src.Repo, token, src.WebhookID, src.WebhookEvents()); err != nil {
				writeMappedError(w, err, "更新 webhook 失败")
				return
			}
		default: // !wantHookNow && hadHook
			if err := s.sources.Client().DeleteWebhook(ctx, src.Owner, src.Repo, token, src.WebhookID); err != nil {
				log.Printf("消息源 %s 删除 webhook 失败: %v", src.Topic(), err)
			}
			src.WebhookID = 0
			src.SecretEnc = ""
		}
	}

	if err := s.db.Save(src).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "消息源保存失败")
		return
	}
	writeJSON(w, http.StatusOK, toSourceDTO(src, user, s.subscribedBy(src.ID, user.ID)))
}

// equalStringSlices 顺序敏感的字符串切片相等比较（事件列表顺序确定）。
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// handleDeleteSource DELETE /api/v1/sources/{id}：删除源与订阅，尽力清理 webhook。
func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	src := s.findSource(w, r)
	if src == nil {
		return
	}
	user := s.currentUser(r)
	if user.ID != src.UserID && user.Role != model.RoleAdmin {
		writeError(w, http.StatusForbidden, "只有消息源创建者或平台管理员可以删除")
		return
	}
	if src.WebhookID != 0 {
		ctx, cancel := contextWithTimeout(r)
		defer cancel()
		if token, err := s.sources.DecryptToken(src.TokenEnc); err == nil {
			if err := s.sources.Client().DeleteWebhook(ctx, src.Owner, src.Repo, token, src.WebhookID); err != nil {
				log.Printf("消息源 %s 清理 webhook 失败（已忽略）: %v", src.Topic(), err)
			}
		}
	}
	if err := s.db.Delete(&model.Source{}, src.ID).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "删除失败")
		return
	}
	s.db.Where("source_id = ?", src.ID).Delete(&model.SourceSub{})
	log.Printf("消息源删除：%s（操作者 uid=%d）", src.Topic(), user.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSubscribeSource POST /api/v1/sources/{id}/subscription：订阅当前源。
func (s *Server) handleSubscribeSource(w http.ResponseWriter, r *http.Request) {
	src := s.findSource(w, r)
	if src == nil {
		return
	}
	user := s.currentUser(r)
	s.db.Clauses(onConflictDoNothing()).Create(&model.SourceSub{SourceID: src.ID, UserID: user.ID, CreatedAt: time.Now()})
	w.WriteHeader(http.StatusNoContent)
}

// handleUnsubscribeSource DELETE /api/v1/sources/{id}/subscription：退订当前源。
func (s *Server) handleUnsubscribeSource(w http.ResponseWriter, r *http.Request) {
	src := s.findSource(w, r)
	if src == nil {
		return
	}
	user := s.currentUser(r)
	s.db.Where("source_id = ? AND user_id = ?", src.ID, user.ID).Delete(&model.SourceSub{})
	w.WriteHeader(http.StatusNoContent)
}

// ---- 内部工具 ----

func (s *Server) findSource(w http.ResponseWriter, r *http.Request) *model.Source {
	id := parseUint(r.PathValue("id"))
	var src model.Source
	if id == 0 || s.db.First(&src, id).Error != nil {
		writeError(w, http.StatusNotFound, "消息源不存在")
		return nil
	}
	return &src
}

func (s *Server) subscribedBy(sourceID, userID uint64) bool {
	var count int64
	s.db.Model(&model.SourceSub{}).Where("source_id = ? AND user_id = ?", sourceID, userID).Count(&count)
	return count > 0
}

// webhookCallbackURL 对外回调地址（系统设置优先于启动参数）。
func (s *Server) webhookCallbackURL() string {
	return strings.TrimSuffix(s.effectiveBaseURL(), "/") + "/api/v1/webhooks/github"
}

// checkWebhookBaseURL webhook 需要公网可达；未配置时给出可定位的错误。
func (s *Server) checkWebhookBaseURL() string {
	u, err := parseURL(s.effectiveBaseURL())
	if err != nil {
		return "尚未配置对外地址，请先在管理台「系统设置」中设置，或通过 -base-url 启动参数指定"
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "当前对外地址是本机地址，GitHub 无法回调；请在管理台「系统设置」中配置公网地址"
	}
	return ""
}

// mapGitHubError 把上游状态码映射为用户可读提示；技术细节只进日志。
func mapGitHubError(err error, fallback string) (int, string) {
	var se *source.StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusUnauthorized:
			return http.StatusBadRequest, "GitHub 访问令牌无效或已过期"
		case http.StatusNotFound:
			return http.StatusBadRequest, "仓库不存在或令牌无权访问"
		case http.StatusForbidden:
			return http.StatusBadRequest, "令牌权限不足，需要仓库读取与 webhook 管理权限"
		}
	}
	log.Printf("GitHub API 调用失败: %v", err)
	return http.StatusBadGateway, fallback
}

// writeMappedError 按映射结果写错误响应。
func writeMappedError(w http.ResponseWriter, err error, fallback string) {
	code, msg := mapGitHubError(err, fallback)
	writeError(w, code, msg)
}

func toSourceDTO(src *model.Source, viewer *model.User, subscribed bool) model.SourceDTO {
	return model.SourceDTO{
		ID: src.ID, Type: src.Type, Owner: src.Owner, Repo: src.Repo, Topic: src.Topic(),
		WatchRelease: src.WatchRelease, WatchIssues: src.WatchIssues, WatchPulls: src.WatchPulls,
		WatchStars: src.WatchStars, WatchForks: src.WatchForks,
		UseWebhook:  src.UseWebhook,
		IntervalSec: src.IntervalSec, LastStars: src.LastStars, LastForks: src.LastForks,
		LastRunAt: src.LastRunAt, LastError: src.LastError, Enabled: src.Enabled,
		WebhookReady: src.WebhookID != 0,
		Mine:         viewer != nil && (viewer.ID == src.UserID || viewer.Role == model.RoleAdmin),
		Subscribed:   subscribed,
		CreatedAt:    src.CreatedAt,
	}
}

func trim(s string) string {
	return strings.TrimSpace(s)
}

// contextWithTimeout 从请求派生带超时的上下文（外呼 GitHub 用）。
func contextWithTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 20*time.Second)
}

func onConflictDoNothing() clause.OnConflict { return clause.OnConflict{DoNothing: true} }

func parseURL(raw string) (*url.URL, error) { return url.Parse(raw) }
