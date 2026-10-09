package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
	"commandcode-cpa-plugin/resources"
)

const poolManagementBodyLimit = 1 << 20

func poolManagementJSON(status int, value any) (pluginapi.ManagementResponse, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"error":"response encoding failed"}`)}, nil
	}
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
}

func poolManagementError(status int, message string) (pluginapi.ManagementResponse, error) {
	return poolManagementJSON(status, map[string]string{"error": message})
}

func decodePoolManagementBody(body []byte, dest any) bool {
	if len(body) > poolManagementBodyLimit {
		return false
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dest) != nil {
		return false
	}
	var extra any
	return decoder.Decode(&extra) == io.EOF
}

func (m *Manager) handleViewResource(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if m.execClosing.Load() {
		return poolManagementError(http.StatusServiceUnavailable, "compatibility view is shutting down")
	}
	if req.Method != http.MethodGet {
		return poolManagementError(http.StatusNotFound, "not found")
	}
	switch req.Path {
	case "/v0/resource/plugins/" + originalPluginID + "/pool":
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: managementResourcePageForPlugin(resources.PoolPage, nextPluginID)}, nil
	case "/v0/resource/plugins/" + originalPluginID + "/codex":
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: managementResourcePageForPlugin(resources.CodexPage, nextPluginID)}, nil
	default:
		return poolManagementError(http.StatusNotFound, "not found")
	}
}

// CRUD 必须先取得 lifeMu 再加入 WG，避免 shutdown 持 lifeMu 等待尚未取得它的请求。
func (m *Manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if pluginRole == "view" {
		return m.handleViewResource(req)
	}
	guardOnly := m.guardOnly()
	if pluginRole == "full" && (pluginName == nextPluginID || pluginName == updatePluginID) && !guardOnly {
		legacyPrefix := "/v0/management/plugins/" + originalPluginID
		if req.Path == legacyPrefix || strings.HasPrefix(req.Path, legacyPrefix+"/") {
			req.Path = "/v0/management/plugins/" + pluginName + strings.TrimPrefix(req.Path, legacyPrefix)
		}
	}
	if req.Method == http.MethodGet && req.Path == "/v0/resource/plugins/"+pluginName+"/pool" {
		if guardOnly {
			return poolManagementError(http.StatusServiceUnavailable, "account pool unavailable in guard-only mode")
		}
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: managementResourcePage(resources.PoolPage)}, nil
	}
	if req.Method == http.MethodGet && req.Path == "/v0/resource/plugins/"+pluginName+"/codex" {
		return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, Body: managementResourcePage(resources.CodexPage)}, nil
	}
	base := "/v0/management/plugins/" + pluginName
	if guardOnly && strings.HasPrefix(req.Path, base+"/") && !strings.HasPrefix(req.Path, base+"/codex/") {
		return poolManagementError(http.StatusServiceUnavailable, "account pool unavailable in guard-only mode")
	}
	if req.Path == base+"/codex/bans" || req.Path == base+"/codex/unban" || req.Path == base+"/codex/unban-all" || req.Path == base+"/codex/bans/import" {
		return m.handleCodexManagement(req)
	}
	crud := req.Method == http.MethodPost && (req.Path == base+"/accounts" || req.Path == base+"/accounts/import" || req.Path == base+"/accounts/delete")
	known := crud || req.Method == http.MethodGet && (req.Path == base+"/accounts" || req.Path == base+"/events" || req.Path == base+"/status") || req.Method == http.MethodPost && req.Path == base+"/quota/refresh"
	if !known {
		return poolManagementError(http.StatusNotFound, "not found")
	}
	if len(req.Body) > poolManagementBodyLimit {
		return poolManagementError(http.StatusRequestEntityTooLarge, "request body too large")
	}
	if crud {
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
	}
	m.mu.RLock()
	if m.execClosing.Load() || m.pool == nil {
		m.mu.RUnlock()
		return poolManagementError(http.StatusServiceUnavailable, "account pool unavailable")
	}
	p, cfg := m.pool, m.cfg
	m.managementWG.Add(1)
	m.mu.RUnlock()
	defer m.managementWG.Done()

	switch {
	case req.Method == http.MethodGet && req.Path == base+"/accounts":
		return poolManagementJSON(http.StatusOK, map[string]any{"accounts": p.Snapshot()})
	case req.Method == http.MethodGet && req.Path == base+"/status":
		return poolManagementJSON(http.StatusOK, map[string]any{"status": map[string]any{
			"plugin_version": pluginVersion, "provider_id": ProviderID, "scope": "single_process",
			"quota_max_age_seconds":          cfg.Pool.QuotaMaxAge.Seconds(),
			"quota_refresh_interval_seconds": cfg.Pool.QuotaRefreshInterval.Seconds(),
			"account_count":                  len(p.Snapshot()), "default_limit": cfg.Pool.DefaultLimit,
			"home_supported": false, "scheduler_policy": "headroom_then_credits",
		}})
	case req.Method == http.MethodGet && req.Path == base+"/events":
		var after uint64
		if values, exists := req.Query["after"]; exists {
			if len(values) != 1 || values[0] == "" {
				return poolManagementError(http.StatusBadRequest, "invalid event cursor")
			}
			var err error
			after, err = strconv.ParseUint(values[0], 10, 64)
			if err != nil {
				return poolManagementError(http.StatusBadRequest, "invalid event cursor")
			}
		}
		var events []pool.Event
		if values, exists := req.Query["scope"]; exists {
			if len(values) != 1 || values[0] != "requests" {
				return poolManagementError(http.StatusBadRequest, "invalid event scope")
			}
			events = p.RequestEvents(after)
		} else {
			events = p.Events(after)
		}
		cursor := after
		for _, event := range events {
			if event.Sequence > cursor {
				cursor = event.Sequence
			}
		}
		return poolManagementJSON(http.StatusOK, map[string]any{"events": events, "next_cursor": cursor})
	case req.Method == http.MethodPost && req.Path == base+"/accounts":
		var input pool.AccountInput
		if !decodePoolManagementBody(req.Body, &input) {
			return poolManagementError(http.StatusBadRequest, "invalid account request")
		}
		account, err := p.Upsert(input)
		if err != nil {
			return poolManagementError(http.StatusBadRequest, "account could not be saved")
		}
		result := map[string]any{"account": account, "accounts": p.Snapshot()}
		m.poolCatalogRefreshResult(ctx, result)
		return poolManagementJSON(http.StatusOK, result)
	case req.Method == http.MethodPost && req.Path == base+"/accounts/import":
		var input struct {
			Accounts []pool.AccountInput `json:"accounts"`
		}
		if !decodePoolManagementBody(req.Body, &input) || len(input.Accounts) == 0 || duplicatePoolAccounts(input.Accounts) {
			return poolManagementError(http.StatusBadRequest, "invalid account import")
		}
		added := make([]pool.AccountView, 0, len(input.Accounts))
		for _, account := range input.Accounts {
			view, err := p.Upsert(account)
			if err != nil {
				result := map[string]any{"accounts": added, "error": "account import failed", "partial_success": len(added) > 0}
				status := http.StatusBadRequest
				if len(added) > 0 {
					m.poolCatalogRefreshResult(ctx, result)
					status = http.StatusOK
				}
				return poolManagementJSON(status, result)
			}
			added = append(added, view)
		}
		result := map[string]any{"accounts": added}
		m.poolCatalogRefreshResult(ctx, result)
		return poolManagementJSON(http.StatusOK, result)
	case req.Method == http.MethodPost && req.Path == base+"/accounts/delete":
		var input struct {
			ID string `json:"id"`
		}
		if !decodePoolManagementBody(req.Body, &input) || strings.TrimSpace(input.ID) == "" {
			return poolManagementError(http.StatusBadRequest, "invalid account delete request")
		}
		if err := p.Delete(input.ID); err != nil {
			return poolManagementError(http.StatusBadRequest, "account could not be deleted")
		}
		result := map[string]any{"deleted": true, "accounts": p.Snapshot()}
		m.poolCatalogRefreshResult(ctx, result)
		return poolManagementJSON(http.StatusOK, result)
	case req.Method == http.MethodPost && req.Path == base+"/quota/refresh":
		var input struct {
			ID string `json:"id"`
		}
		if !decodePoolManagementBody(req.Body, &input) {
			return poolManagementError(http.StatusBadRequest, "invalid quota refresh request")
		}
		if err := m.refreshPoolQuotas(ctx, input.ID); err != nil {
			return poolManagementJSON(http.StatusBadGateway, map[string]any{"accounts": p.Snapshot(), "error": "quota refresh failed"})
		}
		return poolManagementJSON(http.StatusOK, map[string]any{"accounts": p.Snapshot()})
	default:
		return poolManagementError(http.StatusNotFound, "not found")
	}
}

// 账号写入成功与目录刷新失败分别返回，不能以刷新错误掩盖已经完成的 CRUD。
func (m *Manager) poolCatalogRefreshResult(ctx context.Context, result map[string]any) {
	if m.refreshCatalogNow(ctx) != nil {
		result["catalog_refresh_error"] = "catalog refresh failed"
	}
}

func duplicatePoolAccounts(accounts []pool.AccountInput) bool {
	ids, keys := make(map[string]bool, len(accounts)), make(map[string]bool, len(accounts))
	for _, account := range accounts {
		id, key := strings.TrimSpace(account.ID), strings.TrimSpace(account.APIKey)
		if id == "" && key != "" {
			id = authKeyHash(key)
		}
		if id != "" {
			if ids[id] {
				return true
			}
			ids[id] = true
		}
		if key != "" {
			if keys[key] {
				return true
			}
			keys[key] = true
		}
	}
	return false
}
