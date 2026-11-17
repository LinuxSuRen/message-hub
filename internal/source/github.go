package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// RepoInfo GitHub 仓库概要（star/fork 轮询用）。
type RepoInfo struct {
	FullName string
	Stars    int64
	Forks    int64
}

// Release GitHub release 概要。
type Release struct {
	ID      int64
	TagName string
	Name    string
	URL     string
	Body    string
}

// GitHubClient 极简 GitHub REST 客户端；apiURL 可覆盖以适配 GitHub Enterprise 与测试。
type GitHubClient struct {
	apiURL string
	hc     *http.Client
}

// NewGitHubClient 创建客户端，apiURL 空串使用官方地址。
func NewGitHubClient(apiURL string) *GitHubClient {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	return &GitHubClient{apiURL: apiURL, hc: &http.Client{Timeout: 15 * time.Second}}
}

// APIURL 返回当前 API 基地址（测试与诊断用）。
func (c *GitHubClient) APIURL() string { return c.apiURL }

// StatusError 携带 GitHub 返回状态码的错误，调用方据此映射用户可读提示。
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub API %d: %s", e.Status, e.Body)
}

// do 发起请求；out 非空时解析 JSON 响应。
func (c *GitHubClient) do(ctx context.Context, method, path, token string, payload any, out any) error {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &StatusError{Status: resp.StatusCode, Body: truncateRunes(string(body), 200)}
	}
	if out != nil {
		return json.Unmarshal(body, out)
	}
	return nil
}

// GetRepo 查询仓库概要（同时用于校验令牌与仓库可达性）。
func (c *GitHubClient) GetRepo(ctx context.Context, owner, repo, token string) (RepoInfo, error) {
	var raw struct {
		FullName      string `json:"full_name"`
		StargazersCnt int64  `json:"stargazers_count"`
		ForksCount    int64  `json:"forks_count"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, token, nil, &raw); err != nil {
		return RepoInfo{}, err
	}
	return RepoInfo{FullName: raw.FullName, Stars: raw.StargazersCnt, Forks: raw.ForksCount}, nil
}

// LatestRelease 查询最新 release；仓库尚无 release 时返回 nil。
func (c *GitHubClient) LatestRelease(ctx context.Context, owner, repo, token string) (*Release, error) {
	var raw struct {
		ID      int64  `json:"id"`
		TagName string `json:"tag_name"`
		Name    string `json:"name"`
		HTMLURL string `json:"html_url"`
		Body    string `json:"body"`
	}
	err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo+"/releases/latest", token, nil, &raw)
	if se, ok := err.(*StatusError); ok && se.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Release{ID: raw.ID, TagName: raw.TagName, Name: raw.Name, URL: raw.HTMLURL, Body: raw.Body}, nil
}

// CreateWebhook 注册仓库 webhook，返回 hook ID。
func (c *GitHubClient) CreateWebhook(ctx context.Context, owner, repo, token, callbackURL, secret string, events []string) (int64, error) {
	payload := map[string]any{
		"name": "web",
		"active": true,
		"events": events,
		"config": map[string]any{
			"url":          callbackURL,
			"content_type": "json",
			"secret":       secret,
			"insecure_ssl": "0",
		},
	}
	var raw struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/repos/"+owner+"/"+repo+"/hooks", token, payload, &raw); err != nil {
		return 0, err
	}
	return raw.ID, nil
}

// UpdateWebhookEvents 更新已注册 webhook 的关注事件列表。
func (c *GitHubClient) UpdateWebhookEvents(ctx context.Context, owner, repo, token string, hookID int64, events []string) error {
	payload := map[string]any{"events": events}
	return c.do(ctx, http.MethodPatch, "/repos/"+owner+"/"+repo+"/hooks/"+strconv.FormatInt(hookID, 10), token, payload, nil)
}

// DeleteWebhook 删除 webhook。
func (c *GitHubClient) DeleteWebhook(ctx context.Context, owner, repo, token string, hookID int64) error {
	return c.do(ctx, http.MethodDelete, "/repos/"+owner+"/"+repo+"/hooks/"+strconv.FormatInt(hookID, 10), token, nil, nil)
}

// truncateRunes 按字符数截断（日志与错误信息用）。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
