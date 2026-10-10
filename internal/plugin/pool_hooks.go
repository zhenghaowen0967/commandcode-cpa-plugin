package plugin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

const (
	requestIDHeader = "X-Commandcode-Pool-Request-Id"
	traceIDHeader   = "X-Commandcode-Pool-Trace-Id"
)

// v8.0.20 只发布 selected_auth_id，不发布 auth provider；必须查可信池记录，不能按模型猜。
const selectedPoolAuthMetadataKey = "selected_auth_id"

func (m *Manager) handleSchedulerPick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed scheduler request body"), nil
	}
	available, blocked := m.codex.Filter(req.Candidates, time.Now())
	if blocked {
		// 内建委托会重新看到全部候选，不能用于已排除禁用账号的请求。
		return okEnvelope(m.pickAfterCodexFilter(req, available)), nil
	}
	return okEnvelope(m.pickPoolRoute(req)), nil
}

func (m *Manager) pickPoolRoute(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	ids := make([]string, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidate.Provider != ProviderID {
			return pluginapi.SchedulerPickResponse{}
		}
		ids = append(ids, candidate.ID)
	}
	poolRoute := len(ids) > 0 || req.Provider == ProviderID
	if !poolRoute {
		for _, provider := range req.Providers {
			if provider != ProviderID {
				return pluginapi.SchedulerPickResponse{}
			}
			poolRoute = true
		}
	}
	if !poolRoute {
		return pluginapi.SchedulerPickResponse{}
	}
	return m.pickPoolIDs(req, ids)
}

func (m *Manager) pickPoolIDs(req pluginapi.SchedulerPickRequest, ids []string) pluginapi.SchedulerPickResponse {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.execClosing.Load() || m.pool == nil {
		return poolSchedulerReject()
	}
	// 调度不预占并发；占位仍由实际上游I/O入口的 Acquire 执行。
	decision := m.pool.PickWithTrace(ids, poolHookRequestID(req.Options.Headers), req.Model, poolHookTraceID(req.Options.Headers))
	if decision.AuthID == "" {
		return poolSchedulerReject()
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: decision.AuthID}
}

func (m *Manager) pickAfterCodexFilter(req pluginapi.SchedulerPickRequest, available []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	if len(available) == 0 {
		return codexSchedulerReject()
	}
	poolOnly := true
	for _, candidate := range available {
		poolOnly = poolOnly && candidate.Provider == ProviderID
	}
	if poolOnly {
		ids := make([]string, 0, len(available))
		for _, candidate := range available {
			ids = append(ids, candidate.ID)
		}
		return m.pickPoolIDs(req, ids)
	}
	sort.SliceStable(available, func(i, j int) bool { return available[i].Priority > available[j].Priority })
	for start := 0; start < len(available); {
		end := start + 1
		for end < len(available) && available[end].Priority == available[start].Priority {
			end++
		}
		var ids []string
		for _, candidate := range available[start:end] {
			if candidate.Provider == ProviderID {
				ids = append(ids, candidate.ID)
			}
		}
		poolChecked := false
		for _, candidate := range available[start:end] {
			if candidate.Provider != ProviderID {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: candidate.ID}
			}
			if !poolChecked {
				poolChecked = true
				if decision := m.pickPoolIDs(req, ids); !decision.Reject {
					return decision
				}
			}
		}
		start = end
	}
	return codexSchedulerReject()
}

func codexSchedulerReject() pluginapi.SchedulerPickResponse {
	return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "codex_quota_hold", RejectReason: "no eligible account outside Codex quota hold"}
}

func poolSchedulerReject() pluginapi.SchedulerPickResponse {
	return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "pool_unavailable", RejectReason: "no eligible pool account"}
}

func (m *Manager) handleInterceptBefore(request []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed request interception body"), nil
	}
	response := pluginapi.RequestInterceptResponse{ClearHeaders: []string{requestIDHeader, traceIDHeader}}
	if validPoolHookRequestID(req.RequestID) {
		response.Headers = trustedPoolHeaders(req)
	}
	return okEnvelope(response), nil
}

func validPoolHookRequestID(id string) bool {
	return id != "" && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "\r\n")
}

func poolHookRequestID(headers map[string][]string) string {
	var ids []string
	for name, values := range headers {
		if strings.EqualFold(name, requestIDHeader) {
			ids = append(ids, values...)
		}
	}
	if len(ids) != 1 || !validPoolHookRequestID(ids[0]) {
		return ""
	}
	return ids[0]
}

func trustedPoolHeaders(req pluginapi.RequestInterceptRequest) http.Header {
	headers := http.Header{requestIDHeader: {req.RequestID}}
	// TraceID 只作观测关联，缺失时保留旧宿主执行路径，不回退客户端值。
	if trace := pool.NormalizeTraceID(req.TraceID); trace != "" {
		headers[traceIDHeader] = []string{trace}
	}
	return headers
}

func poolHookTraceID(headers map[string][]string) string {
	var ids []string
	for name, values := range headers {
		if strings.EqualFold(name, traceIDHeader) {
			ids = append(ids, values...)
		}
	}
	if len(ids) != 1 {
		return ""
	}
	return pool.NormalizeTraceID(ids[0])
}

func (m *Manager) handleInterceptAfter(request []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed request interception body"), nil
	}
	response := pluginapi.RequestInterceptResponse{ClearHeaders: []string{requestIDHeader, traceIDHeader}}
	authID, _ := req.Metadata[selectedPoolAuthMetadataKey].(string)
	if authID == "" {
		return okEnvelope(response), nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.pool == nil {
		return okEnvelope(response), nil
	}
	_, poolAuth := m.pool.Credential(authID)
	if !poolAuth {
		return okEnvelope(response), nil
	}
	// RequestID 只能取宿主字段，不得回退到 metadata、trace 或客户端值。
	if m.execClosing.Load() || !validPoolHookRequestID(req.RequestID) {
		response.Terminate = true
		response.StatusCode = http.StatusServiceUnavailable
		response.ResponseHeaders = http.Header{"Content-Type": {"application/json"}}
		response.ResponseBody = []byte(`{"error":"pool request identity unavailable"}`)
		return okEnvelope(response), nil
	}
	response.Headers = trustedPoolHeaders(req)
	return okEnvelope(response), nil
}

func (m *Manager) handleRequestComplete(request []byte) ([]byte, error) {
	var req pluginapi.RequestCompletion
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed request completion body"), nil
	}
	// completion 仅取消请求；并发占位必须等执行路径确认上游终止后再结算。
	if req.RequestID != "" {
		m.mu.RLock()
		if m.pool != nil {
			m.pool.AbortRequestWithTrace(req.RequestID, req.TraceID)
		}
		m.mu.RUnlock()
	}
	return okEnvelope(struct{}{}), nil
}
