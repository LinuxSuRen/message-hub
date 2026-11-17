package source

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// EventItem 从平台事件提取的待转存消息内容。
type EventItem struct {
	ExternalID string // 源内唯一标识，用于去重
	Title      string
	Body       string
	Level      string
}

// GitHub webhook 载荷中的关键字段（只取用到的部分）。
type ghPayload struct {
	Action      string `json:"action"`
	Release     *ghRelease
	Issue       *ghIssue
	PullRequest *ghPull  `json:"pull_request"`
	Repository  *ghRepo
}

type ghRelease struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
}

type ghIssue struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	User    *ghUser
}

type ghPull struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Merged  bool   `json:"merged"`
	User    *ghUser
}

type ghUser struct {
	Login string `json:"login"`
}

type ghRepo struct {
	FullName string `json:"full_name"`
}

// ParseEvent 把 GitHub webhook 事件转为待转存条目；返回 nil 表示无需转存
// （ping、未知事件、不关心的 action 等）。event 为 X-GitHub-Event 值。
func ParseEvent(event string, payload []byte) *EventItem {
	switch event {
	case "release":
		return parseRelease(payload)
	case "issues":
		return parseIssue(payload)
	case "pull_request":
		return parsePull(payload)
	default:
		return nil
	}
}

// ParseRepoFullName 从载荷提取仓库全名（webhook 回源定位用）。
func ParseRepoFullName(payload []byte) string {
	var p ghPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if p.Repository == nil {
		return ""
	}
	return p.Repository.FullName
}

func parseRelease(payload []byte) *EventItem {
	var p ghPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Release == nil {
		return nil
	}
	if p.Action != "published" {
		return nil
	}
	r := p.Release
	name := releaseDisplayName(r)
	body := r.Body
	if body != "" {
		body = truncateRunes(body, 2000)
	}
	return &EventItem{
		ExternalID: fmt.Sprintf("release:published:%d", r.ID),
		Title:      fmt.Sprintf("发布 %s", name),
		Body:       strings.TrimSpace(body + "\n" + r.HTMLURL),
	}
}

func parseIssue(payload []byte) *EventItem {
	var p ghPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Issue == nil {
		return nil
	}
	switch p.Action {
	case "opened", "closed", "reopened":
	default:
		return nil
	}
	it := p.Issue
	actionText := map[string]string{
		"opened": "新 issue", "closed": "issue 已关闭", "reopened": "issue 重新打开",
	}[p.Action]
	by := ""
	if it.User != nil {
		by = "by " + it.User.Login + " "
	}
	return &EventItem{
		ExternalID: fmt.Sprintf("issues:%s:%d", p.Action, it.ID),
		Title:      fmt.Sprintf("#%d %s %s", it.Number, actionText, it.Title),
		Body:       strings.TrimSpace(by + it.HTMLURL),
	}
}

func parsePull(payload []byte) *EventItem {
	var p ghPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.PullRequest == nil {
		return nil
	}
	pr := p.PullRequest
	// 合并是 closed 的一种：区分文案但共用 closed 这个 externalID。
	if p.Action == "closed" {
		actionText := "PR 已关闭"
		externalID := fmt.Sprintf("pull:closed:%d", pr.ID)
		if pr.Merged {
			actionText = "PR 已合并"
			externalID = fmt.Sprintf("pull:merged:%d", pr.ID)
		}
		return &EventItem{
			ExternalID: externalID,
			Title:      fmt.Sprintf("#%d %s %s", pr.Number, actionText, pr.Title),
			Body:       strings.TrimSpace(pr.HTMLURL),
		}
	}
	if p.Action != "opened" && p.Action != "reopened" {
		return nil
	}
	actionText := map[string]string{"opened": "新 PR", "reopened": "PR 重新打开"}[p.Action]
	by := ""
	if pr.User != nil {
		by = "by " + pr.User.Login + " "
	}
	return &EventItem{
		ExternalID: fmt.Sprintf("pull:%s:%d", p.Action, pr.ID),
		Title:      fmt.Sprintf("#%d %s %s", pr.Number, actionText, pr.Title),
		Body:       strings.TrimSpace(by + pr.HTMLURL),
	}
}

// releaseDisplayName release 展示名：优先 tag，缺省用 tag 名本身。
func releaseDisplayName(r *ghRelease) string {
	if r.Name != "" {
		return r.Name
	}
	return r.TagName
}

// clampMessage 校正标题与正文长度，满足消息表约束。
func (i *EventItem) clampMessage() {
	i.Title = truncateRunes(strings.TrimSpace(i.Title), 128)
	if utf8.RuneCountInString(i.Title) == 0 {
		i.Title = "(无标题)"
	}
	i.Body = truncateRunes(i.Body, 4096)
}
