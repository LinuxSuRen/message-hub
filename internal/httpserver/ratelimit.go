package httpserver

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// tokenLimiter 按令牌维度做每分钟配额的令牌桶限流。
type tokenLimiter struct {
	mu       sync.Mutex
	limiters map[uint64]*rate.Limiter
}

func newTokenLimiter() *tokenLimiter {
	return &tokenLimiter{limiters: map[uint64]*rate.Limiter{}}
}

// allow 判断令牌本次推送是否放行；perMinute 为该令牌的每分钟配额。
func (l *tokenLimiter) allow(tokenID uint64, perMinute int) bool {
	if perMinute < 1 {
		perMinute = 1
	}
	l.mu.Lock()
	lim, ok := l.limiters[tokenID]
	if !ok {
		lim = rate.NewLimiter(rate.Every(time.Minute/time.Duration(perMinute)), perMinute)
		l.limiters[tokenID] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

// remove 令牌吊销后清理对应限流器。
func (l *tokenLimiter) remove(tokenID uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.limiters, tokenID)
}

// removeAll 清理某用户全部令牌的限流器（用户被删除时调用）。
func (l *tokenLimiter) removeAll(tokenIDs []uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range tokenIDs {
		delete(l.limiters, id)
	}
}

// keyedLimiter 通用按键限流器（如按来源 IP），用于认证端点的暴力破解防护。
// 惰性清理：条目数超过上限时移除超过 idle 未活跃的键，防止 map 无界增长。
type keyedLimiter struct {
	mu        sync.Mutex
	entries   map[string]*keyedEntry
	perMinute int
	maxKeys   int
	idleTTL   time.Duration
}

type keyedEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newKeyedLimiter(perMinute, maxKeys int, idleTTL time.Duration) *keyedLimiter {
	if perMinute < 1 {
		perMinute = 1
	}
	return &keyedLimiter{
		entries:   map[string]*keyedEntry{},
		perMinute: perMinute,
		maxKeys:   maxKeys,
		idleTTL:   idleTTL,
	}
}

// allow 判断本次请求是否放行。
func (l *keyedLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, ok := l.entries[key]; ok {
		entry.lastSeen = now
		return entry.limiter.Allow()
	}
	if len(l.entries) >= l.maxKeys {
		l.pruneLocked(now)
	}
	lim := rate.NewLimiter(rate.Every(time.Minute/time.Duration(l.perMinute)), l.perMinute)
	l.entries[key] = &keyedEntry{limiter: lim, lastSeen: now}
	return lim.Allow()
}

// pruneLocked 移除 idle 过期的条目；若清理后仍满，则牺牲精度移除最旧条目。
func (l *keyedLimiter) pruneLocked(now time.Time) {
	for key, entry := range l.entries {
		if now.Sub(entry.lastSeen) > l.idleTTL {
			delete(l.entries, key)
		}
	}
	if len(l.entries) < l.maxKeys {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.entries {
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey, oldest = key, entry.lastSeen
		}
	}
	if oldestKey != "" {
		delete(l.entries, oldestKey)
	}
}

// clientIP 提取对端 IP。仅取 RemoteAddr：X-Forwarded-For 可被伪造，
// 反代场景请通过网络层（如 nginx limit_req）配套防护。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limitByIP 返回按 IP 限流的中间件。
func limitByIP(limiter *keyedLimiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
			return
		}
		next(w, r)
	}
}
