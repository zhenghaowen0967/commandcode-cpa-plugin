// Package codexguard 在进程内记录 Codex 限流封禁。
package codexguard

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	codexProvider       = "codex"
	statusTooMany       = 429
	primaryWindow5h     = 300
	secondaryWindowWeek = 10080
	maxResetUnix        = int64(253402300799) // RFC3339 可表示的 Unix 秒上限
	fullPercent         = 100
	fallbackDuration    = 5 * time.Hour
)

// Guard 的零值可用，封禁仅保存在当前进程内。
type Guard struct {
	mu   sync.Mutex
	bans map[string]Ban
}

// Ban 仅含封禁所需的标识与时间信息。
type Ban struct {
	AuthID   string    `json:"auth_id"`
	ResetAt  time.Time `json:"reset_at"`
	Window   string    `json:"window"`
	BannedAt time.Time `json:"banned_at"`
}

// Record 仅记录宿主确认的 Codex OAuth 失败429；不保存响应正文或凭据。
func (g *Guard) Record(record pluginapi.UsageRecord, now time.Time) bool {
	if !strings.EqualFold(record.Provider, codexProvider) || !strings.EqualFold(record.AuthType, "oauth") || !record.Failed || record.Failure.StatusCode != statusTooMany {
		return false
	}

	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return false
	}

	ban := classify(record.ResponseHeaders, now)
	ban.AuthID = authID
	ban.BannedAt = now

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.bans == nil {
		g.bans = make(map[string]Ban)
	}
	if existing, ok := g.bans[authID]; ok && now.Before(existing.ResetAt) && !ban.ResetAt.After(existing.ResetAt) {
		// 重复 429 不得缩短尚未到期的封禁。
		return true
	}
	g.bans[authID] = ban
	return true
}

// Import 合并迁入的 holds；只新增或延长，不会缩短或删除既有记录。
// 输入应由管理层先完成整批校验；过期项被忽略。
func (g *Guard) Import(bans []Ban, now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()

	changed := make(map[string]struct{}, len(bans))
	for _, ban := range bans {
		if !now.Before(ban.ResetAt) {
			continue
		}
		if existing, ok := g.bans[ban.AuthID]; ok && !ban.ResetAt.After(existing.ResetAt) {
			continue
		}
		if g.bans == nil {
			g.bans = make(map[string]Ban)
		}
		ban.BannedAt = now
		g.bans[ban.AuthID] = ban
		changed[ban.AuthID] = struct{}{}
	}
	return len(changed)
}

// Filter 排除仍处于封禁期的 Codex 候选；blocked 表示至少排除一个候选。
func (g *Guard) Filter(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) (available []pluginapi.SchedulerAuthCandidate, blocked bool) {
	available = make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))

	g.mu.Lock()
	defer g.mu.Unlock()
	for _, candidate := range candidates {
		// 宿主会脱敏api_key；auth_kind保留可信类型，不能按模型或域名推断。
		kind := candidate.Attributes["auth_kind"]
		apiKey := strings.EqualFold(kind, "apikey") || strings.EqualFold(kind, "api_key") || kind == "" && candidate.Attributes["api_key"] != ""
		if !strings.EqualFold(candidate.Provider, codexProvider) || apiKey {
			available = append(available, candidate)
			continue
		}

		ban, ok := g.bans[candidate.ID]
		if !ok {
			available = append(available, candidate)
			continue
		}
		if !now.Before(ban.ResetAt) {
			delete(g.bans, candidate.ID)
			available = append(available, candidate)
			continue
		}
		blocked = true
	}
	return available, blocked
}

// Snapshot 返回按 AuthID 排序的有效封禁，并清除已过期记录。
func (g *Guard) Snapshot(now time.Time) []Ban {
	g.mu.Lock()
	defer g.mu.Unlock()

	bans := make([]Ban, 0, len(g.bans))
	for authID, ban := range g.bans {
		if !now.Before(ban.ResetAt) {
			delete(g.bans, authID)
			continue
		}
		bans = append(bans, ban)
	}
	sort.Slice(bans, func(i, j int) bool { return bans[i].AuthID < bans[j].AuthID })
	return bans
}

// Unban 删除指定 AuthID 的封禁。
func (g *Guard) Unban(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.bans[authID]; !ok {
		return false
	}
	delete(g.bans, authID)
	return true
}

// ClearAll 清空所有封禁并返回删除数量。
func (g *Guard) ClearAll() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	count := len(g.bans)
	g.bans = nil
	return count
}

type windowHeaders struct {
	usedPercent float64
	percentOK   bool
	resetAt     time.Time
	resetOK     bool
	minutes     int64
	minutesOK   bool
}

func classify(headers http.Header, now time.Time) Ban {
	primary := readWindow(headers, "primary", now)
	secondary := readWindow(headers, "secondary", now)

	primaryFull := primary.percentOK && primary.usedPercent >= fullPercent
	secondaryFull := secondary.percentOK && secondary.usedPercent >= fullPercent

	switch {
	case primaryFull && secondaryFull:
		// 双窗均满时，取有效重置时间中的较晚值。
		var reset time.Time
		if primary.resetOK {
			reset = primary.resetAt
		}
		if secondary.resetOK && (reset.IsZero() || secondary.resetAt.After(reset)) {
			reset = secondary.resetAt
		}
		if !reset.IsZero() {
			return Ban{ResetAt: reset, Window: "both"}
		}
	case primaryFull:
		if primary.resetOK {
			return Ban{ResetAt: primary.resetAt, Window: "5h"}
		}
	case secondaryFull:
		if secondary.resetOK {
			return Ban{ResetAt: secondary.resetAt, Window: "week"}
		}
	default:
		// 未指示满额窗口时，按 window-minutes 和有效 reset 识别窗口。
		if primary.resetOK && primary.minutesOK && primary.minutes == primaryWindow5h {
			return Ban{ResetAt: primary.resetAt, Window: "5h"}
		}
		if secondary.resetOK && secondary.minutesOK && secondary.minutes == secondaryWindowWeek {
			return Ban{ResetAt: secondary.resetAt, Window: "week"}
		}
	}

	return Ban{ResetAt: now.Add(fallbackDuration), Window: "fallback-5h"}
}

func readWindow(headers http.Header, name string, now time.Time) windowHeaders {
	prefix := "x-codex-" + name + "-"
	var window windowHeaders
	if raw, ok := singleHeader(headers, prefix+"used-percent"); ok {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 {
			window.usedPercent = value
			window.percentOK = true
		}
	}
	if raw, ok := singleHeader(headers, prefix+"reset-at"); ok {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 && seconds <= maxResetUnix {
			resetAt := time.Unix(seconds, 0)
			if resetAt.After(now) {
				window.resetAt = resetAt
				window.resetOK = true
			}
		}
	}
	if raw, ok := singleHeader(headers, prefix+"window-minutes"); ok {
		if minutes, err := strconv.ParseInt(raw, 10, 64); err == nil {
			window.minutes = minutes
			window.minutesOK = true
		}
	}
	return window
}

// singleHeader 按大小写不敏感查找，并拒绝重复键或多值字段。
func singleHeader(headers http.Header, name string) (string, bool) {
	var value string
	found := false
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		if found || len(values) != 1 {
			return "", false
		}
		value = strings.TrimSpace(values[0])
		if value == "" {
			return "", false
		}
		found = true
	}
	return value, found
}
