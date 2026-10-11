package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPoolHookSessionKey(t *testing.T) {
	long := strings.Repeat("s", 257)
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{"nil", nil, ""},
		{"absent", map[string]any{"other": "x"}, ""},
		{"empty", map[string]any{canonicalSessionMetadataKey: ""}, ""},
		{"blank", map[string]any{canonicalSessionMetadataKey: "   "}, ""},
		{"non-string", map[string]any{canonicalSessionMetadataKey: 7}, ""},
		{"oversized", map[string]any{canonicalSessionMetadataKey: long}, ""},
		{"valid", map[string]any{canonicalSessionMetadataKey: " pck:abc "}, "pck:abc"},
	} {
		if got := poolHookSessionKey(tc.metadata); got != tc.want {
			t.Fatalf("%s: poolHookSessionKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 会话元数据 → 粘性选择：同一会话固定同一账号，新会话与无会话请求按策略。
func TestSchedulerPickHonoursSessionStickiness(t *testing.T) {
	m := newPoolHandlerManager(t)
	a := addPoolHandlerAccount(t, m, "sticky-hook-a", "Account A", "group-a")
	b := addPoolHandlerAccount(t, m, "sticky-hook-b", "Account B", "group-b")
	makePoolHandlerEligible(t, m, a, 0.2, 10)
	makePoolHandlerEligible(t, m, b, 0.9, 10)
	if err := m.pool.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}

	pick := func(requestID, session string) pluginapi.SchedulerPickResponse {
		t.Helper()
		options := pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {requestID}}}
		if session != "" {
			options.Metadata = map[string]any{canonicalSessionMetadataKey: session}
		}
		request := pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: "any-model",
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: a.AuthID, Provider: ProviderID}, {ID: b.AuthID, Provider: ProviderID}},
			Options:    options,
		}
		var got pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
		return got
	}

	if got := pick("sticky-session-1", "sess-1"); !got.Handled || got.AuthID != b.AuthID {
		t.Fatalf("first pick = %+v, want %s (highest headroom)", got, b.AuthID)
	}
	// A 现在余量最高，但 sess-1 已绑定 B。
	makePoolHandlerEligible(t, m, a, 1, 10)
	if got := pick("sticky-session-2", "sess-1"); got.AuthID != b.AuthID {
		t.Fatalf("sticky pick = %s, want %s", got.AuthID, b.AuthID)
	}
	if got := pick("sticky-session-3", "sess-2"); got.AuthID != a.AuthID {
		t.Fatalf("fresh session pick = %s, want %s (strategy)", got.AuthID, a.AuthID)
	}
	if got := pick("sticky-session-4", ""); got.AuthID != a.AuthID {
		t.Fatalf("keyless pick = %s, want %s (strategy)", got.AuthID, a.AuthID)
	}
}

func TestPoolManagementStatusReportsStickyTTL(t *testing.T) {
	m := newPoolHandlerManager(t)
	read := func() float64 {
		t.Helper()
		resp := callPoolManagement(t, m, http.MethodGet, "/status", "")
		var got struct {
			Status struct {
				StickyTTL float64 `json:"sticky_ttl_seconds"`
				Policy    string  `json:"scheduler_policy"`
			} `json:"status"`
		}
		if json.Unmarshal(resp.Body, &got) != nil {
			t.Fatalf("status = %s", resp.Body)
		}
		return got.Status.StickyTTL
	}
	if got := read(); got != 0 {
		t.Fatalf("default sticky ttl = %v, want 0", got)
	}
	if err := m.pool.SetStickyTTL(90 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != 5400 {
		t.Fatalf("sticky ttl = %v, want 5400", got)
	}
	// 配置了 sticky-ttl 的注册也应生效。
	m2, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m2.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	yamlText := testValidYAML + "pool:\n  sticky-ttl: \"2h\"\n"
	if env := decodeEnv(t, mustHandle(t, m2, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, yamlText))); !env.OK {
		t.Fatalf("register rejected sticky-ttl: %+v", env.Error)
	}
	if got := m2.pool.StickyTTL(); got != 2*time.Hour {
		t.Fatalf("registered sticky ttl = %s", got)
	}
}
