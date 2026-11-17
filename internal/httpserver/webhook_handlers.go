package httpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/linuxsuren/message-hub/internal/model"
	"github.com/linuxsuren/message-hub/internal/source"
)

const webhookBodyLimit = 2 << 20 // GitHub 载荷上限 2MB

// handleGitHubWebhook POST /api/v1/webhooks/github：接收 GitHub 仓库事件。
// 认证依赖每源独立的 HMAC-SHA256 签名（X-Hub-Signature-256），不走会话。
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, webhookBodyLimit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "读取请求体失败")
		return
	}
	if len(body) > webhookBodyLimit {
		writeError(w, http.StatusRequestEntityTooLarge, "请求体过大")
		return
	}

	src := s.locateSource(r, body)
	if src == nil {
		writeError(w, http.StatusNotFound, "消息源不存在")
		return
	}
	if !src.Enabled {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	secret, err := s.sources.DecryptToken(src.SecretEnc)
	if err != nil || secret == "" {
		log.Printf("webhook 源 %s 验签密钥缺失", src.Topic())
		writeError(w, http.StatusUnauthorized, "验签失败")
		return
	}
	if !verifyGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		log.Printf("webhook 源 %s 签名校验未通过", src.Topic())
		writeError(w, http.StatusUnauthorized, "验签失败")
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "release", "issues", "pull_request":
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if !eventWatched(src, event) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	item := source.ParseEvent(event, body)
	if item == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	item.Level = model.LevelInfo
	created, err := s.sources.Emit(src, item)
	if err != nil {
		log.Printf("webhook 源 %s 事件转存失败: %v", src.Topic(), err)
		writeError(w, http.StatusInternalServerError, "处理失败")
		return
	}
	if created {
		log.Printf("webhook 源 %s 转存事件 %s", src.Topic(), item.ExternalID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "created": created})
}

// locateSource 优先按 X-GitHub-Hook-ID 定位源，缺失时按载荷仓库全名兜底。
func (s *Server) locateSource(r *http.Request, body []byte) *model.Source {
	if raw := r.Header.Get("X-GitHub-Hook-ID"); raw != "" {
		if hookID, err := strconv.ParseInt(raw, 10, 64); err == nil {
			var src model.Source
			if err := s.db.Where("webhook_id = ?", hookID).First(&src).Error; err == nil {
				return &src
			}
		}
	}
	fullName := source.ParseRepoFullName(body)
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return nil
	}
	var src model.Source
	if err := s.db.Where("type = ? AND owner = ? AND repo = ?", "github", owner, repo).First(&src).Error; err != nil {
		return nil
	}
	return &src
}

// eventWatched 源的关注开关是否包含该事件。
func eventWatched(src *model.Source, event string) bool {
	switch event {
	case "release":
		return src.WatchRelease
	case "issues":
		return src.WatchIssues
	case "pull_request":
		return src.WatchPulls
	}
	return false
}

// verifyGitHubSignature 常数时间比较 sha256= 前缀的 HMAC 签名。
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := mac.Sum(nil)
	given, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	return hmac.Equal(expected, given)
}
