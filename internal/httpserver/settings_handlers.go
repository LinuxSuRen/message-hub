package httpserver

import (
	"log"
	"net/http"
	"strings"

	"github.com/linuxsuren/message-hub/internal/model"
)

// 系统设置：当前仅「对外地址」，供 GitHub webhook 回调使用。
// 取值顺序：系统设置（DB）> -base-url 启动参数。

// handleGetSettings GET /api/v1/settings：登录即可读（建源时需要知道是否已配置公网地址）。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	fromSetting := s.settingValue(model.SettingBaseURL) != ""
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":             s.effectiveBaseURL(),
		"base_url_configured":  fromSetting,
	})
}

// handleUpdateSettings PUT /api/v1/settings {"base_url":"https://msg.example.com"}（平台管理员）。
// 空串表示清除，回落到启动参数。
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base != "" {
		u, err := parseURL(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			writeError(w, http.StatusBadRequest, "对外地址需要是 http(s):// 开头的完整地址")
			return
		}
	}
	if err := s.db.Save(&model.Setting{Key: model.SettingBaseURL, Value: base}).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "设置保存失败")
		return
	}
	log.Printf("系统设置更新：对外地址 = %q（操作者 uid=%d）", base, s.currentUser(r).ID)
	writeJSON(w, http.StatusOK, map[string]any{"base_url": base})
}

// settingValue 读取系统设置；不存在返回空串。
func (s *Server) settingValue(key string) string {
	var st model.Setting
	if err := s.db.Where("key = ?", key).First(&st).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(st.Value)
}

// effectiveBaseURL 系统设置优先于启动参数。
func (s *Server) effectiveBaseURL() string {
	if v := s.settingValue(model.SettingBaseURL); v != "" {
		return v
	}
	return s.cfg.BaseURL
}
