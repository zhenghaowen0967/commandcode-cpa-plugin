package plugin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/codexguard"
)

func (m *Manager) handleCodexUsage(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if json.Unmarshal(request, &record) != nil {
		return ErrEnvelope("invalid_request", "malformed usage request body"), nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.execClosing.Load() {
		m.codex.Record(record, time.Now())
		m.usageEvents.Add(1)
	}
	return okEnvelope(struct{}{}), nil
}

func (m *Manager) handleCodexManagement(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	base := "/v0/management/plugins/" + pluginName + "/codex"
	known := req.Method == http.MethodGet && req.Path == base+"/bans" || req.Method == http.MethodPost && (req.Path == base+"/bans/import" || req.Path == base+"/unban" || req.Path == base+"/unban-all")
	if !known {
		return poolManagementError(http.StatusNotFound, "not found")
	}
	if len(req.Body) > poolManagementBodyLimit {
		return poolManagementError(http.StatusRequestEntityTooLarge, "request body too large")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.execClosing.Load() {
		return poolManagementError(http.StatusServiceUnavailable, "plugin is closing")
	}
	result := map[string]any{
		"scope": "single_process", "state": "memory_only", "host_cooldown_independent": true,
		"usage_events_seen": m.usageEvents.Load(), "guard_only": m.cfg.GuardOnly,
		"pool_active": m.pool != nil, "activation_error": m.poolActivationError,
	}
	if req.Method == http.MethodPost {
		switch req.Path {
		case base + "/unban":
			var input struct {
				AuthID string `json:"auth_id"`
			}
			if !decodePoolManagementBody(req.Body, &input) || !validCodexAuthID(input.AuthID) {
				return poolManagementError(http.StatusBadRequest, "invalid auth identity")
			}
			result["cleared"] = m.codex.Unban(input.AuthID)
		case base + "/bans/import":
			bans, ok := decodeCodexBanImport(req.Body)
			if !ok {
				return poolManagementError(http.StatusBadRequest, "invalid Codex hold import")
			}
			result["imported_count"] = m.codex.Import(bans, time.Now())
		default:
			var input struct{}
			if !decodePoolManagementBody(req.Body, &input) {
				return poolManagementError(http.StatusBadRequest, "invalid unban request")
			}
			result["cleared_count"] = m.codex.ClearAll()
		}
	}
	result["bans"] = m.codex.Snapshot(time.Now())
	return poolManagementJSON(http.StatusOK, result)
}

type codexBanImportRequest struct {
	// These wrapper fields are present in the legacy /bans response and are
	// accepted only to allow a caller to forward that response unchanged.
	Plugin  string                 `json:"plugin"`
	Version string                 `json:"version"`
	Count   int                    `json:"count"`
	Bans    []codexBanImportRecord `json:"bans"`
}

type codexBanImportRecord struct {
	AuthID           string          `json:"auth_id"`
	ResetAt          json.RawMessage `json:"reset_at"`
	Window           string          `json:"window"`
	BannedAt         string          `json:"banned_at"`
	BannedAtUnix     int64           `json:"banned_at_unix"`
	ResetAtUnix      int64           `json:"reset_at_unix"`
	RemainingSeconds int64           `json:"remaining_seconds"`
}

func decodeCodexBanImport(body []byte) ([]codexguard.Ban, bool) {
	var input codexBanImportRequest
	if !decodePoolManagementBody(body, &input) || input.Bans == nil {
		return nil, false
	}

	bans := make([]codexguard.Ban, 0, len(input.Bans))
	for _, record := range input.Bans {
		if !validCodexAuthID(record.AuthID) || (record.Window != "" && (len(record.Window) > 128 || hasControlRune(record.Window))) {
			return nil, false
		}
		resetAt, ok := parseCodexResetAt(record.ResetAt)
		if !ok {
			return nil, false
		}
		// The window is intentionally generic: imported state is held through
		// reset_at without trusting legacy classification metadata.
		bans = append(bans, codexguard.Ban{AuthID: record.AuthID, ResetAt: resetAt, Window: "imported"})
	}
	return bans, true
}

func validCodexAuthID(authID string) bool {
	return authID != "" && authID == strings.TrimSpace(authID) && len(authID) <= 1024 && !hasControlRune(authID)
}

func hasControlRune(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func parseCodexResetAt(raw json.RawMessage) (time.Time, bool) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return time.Time{}, false
	}

	var resetAt time.Time
	if strings.HasPrefix(value, "\"") {
		var timestamp string
		if json.Unmarshal(raw, &timestamp) != nil {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return time.Time{}, false
		}
		resetAt = parsed
	} else {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < -62135596800 || seconds > 253402300799 {
			return time.Time{}, false
		}
		resetAt = time.Unix(seconds, 0).UTC()
	}
	unix := resetAt.Unix()
	if resetAt.UTC().Year() < 1 || resetAt.UTC().Year() > 9999 || unix < -62135596800 || unix > 253402300799 {
		return time.Time{}, false
	}
	return resetAt, true
}
