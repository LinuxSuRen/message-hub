package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// SessionCookie 会话 Cookie 名。
const SessionCookie = "mh_session"

// SessionTTL 会话有效期。
const SessionTTL = 12 * time.Hour

// SessionManager 签发与校验无状态会话（HMAC-SHA256 签名，无需服务端存储）。
// 载荷携带 per-user 世代号（epoch）：重置密码等安全事件自增后，
// 该用户全部已签发会话立即失效，弥补无状态会话无法主动吊销的缺口。
type SessionManager struct {
	secret []byte
}

// NewSessionManager 创建会话管理器。
func NewSessionManager(secret []byte) *SessionManager {
	return &SessionManager{secret: secret}
}

// Issue 为用户签发会话值：base64url(uid.epoch.exp) + "." + hex(hmac)。
func (m *SessionManager) Issue(userID, epoch uint64) string {
	payload := fmt.Sprintf("%d.%d.%d", userID, epoch, time.Now().Add(SessionTTL).Unix())
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + hex.EncodeToString(sign(m.secret, payload))
}

// Verify 校验会话值，返回用户 ID 与签发时的世代号。
func (m *SessionManager) Verify(value string) (userID, epoch uint64, ok bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, 0, false
	}
	payload := string(raw)
	got, err := hex.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(sign(m.secret, payload), got) != 1 {
		return 0, 0, false
	}
	fields := strings.Split(payload, ".")
	if len(fields) != 3 {
		return 0, 0, false
	}
	uid, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	ep, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, 0, false
	}
	return uid, ep, true
}

// SetCookie 向响应写入会话 Cookie；secure 表示仅在 HTTPS 下回传。
func (m *SessionManager) SetCookie(w http.ResponseWriter, userID, epoch uint64, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    m.Issue(userID, epoch),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

// ClearCookie 清除会话 Cookie。
func (m *SessionManager) ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func sign(secret []byte, data string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
