package plugin

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/codexguard"
)

func TestCodexAPIKey429LeavesBuiltinBackoffInCharge(t *testing.T) {
	for _, model := range []string{"glm-5.3", "glm-5.3-flash", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			m := NewManager(nil)
			r := pluginapi.UsageRecord{Provider: "codex", AuthType: "apikey", AuthID: "bigmodel-static", Model: model, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}, ResponseHeaders: http.Header{"Retry-After": {"2"}}}
			mustHandle(t, m, pluginabi.MethodUsageHandle, poolHookBody(t, r))
			if len(m.codex.Snapshot(time.Now())) != 0 {
				t.Fatal("API-key 429 acquired a Codex quota hold")
			}
			candidate := pluginapi.SchedulerAuthCandidate{ID: r.AuthID, Provider: "codex", Attributes: map[string]string{"api_key": "public-synthetic-key"}}
			got := unifiedPick(t, m, candidate)
			if got.Handled || got.Reject || got.AuthID != "" || got.DelegateBuiltin != "" {
				t.Fatalf("third-party backoff was overridden: %+v", got)
			}
		})
	}
}

func TestUnifiedSchedulerHistoricalAPIKeyHoldCannotBlockGLM(t *testing.T) {
	m := NewManager(nil)
	now := time.Now()
	m.codex.Import([]codexguard.Ban{{AuthID: "bigmodel-static", ResetAt: now.Add(5 * time.Hour)}}, now)
	apiKey := pluginapi.SchedulerAuthCandidate{ID: "bigmodel-static", Provider: "codex", Attributes: map[string]string{"auth_kind": "apikey"}}
	if got := unifiedPick(t, m, apiKey); got.Handled || got.Reject {
		t.Fatalf("historical false hold blocked third-party request: %+v", got)
	}
	holdCodex(t, m, "genuine-oauth")
	oauth := pluginapi.SchedulerAuthCandidate{ID: "genuine-oauth", Provider: "codex", Priority: 100}
	if got := unifiedPick(t, m, oauth); !got.Handled || !got.Reject || got.RejectCode != "codex_quota_hold" || got.DelegateBuiltin != "" {
		t.Fatalf("genuine OAuth escaped protection: %+v", got)
	}
	if got := unifiedPick(t, m, oauth, apiKey); !got.Handled || got.Reject || got.AuthID != apiKey.ID || got.DelegateBuiltin != "" {
		t.Fatalf("mixed routing ignored trusted API-key identity: %+v", got)
	}
}

func TestTargetedAPIKeyUnbanDoesNotClearGenuineCodexHold(t *testing.T) {
	m := NewManager(nil)
	holdCodex(t, m, "genuine-oauth")
	now := time.Now()
	m.codex.Import([]codexguard.Ban{{AuthID: "bigmodel-static", ResetAt: now.Add(5 * time.Hour)}}, now)
	before := m.codex.Snapshot(now)
	resp := callPoolManagement(t, m, http.MethodPost, "/codex/unban", `{"auth_id":"bigmodel-static"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"cleared":true`) {
		t.Fatalf("targeted unban failed: %d", resp.StatusCode)
	}
	after := m.codex.Snapshot(now)
	if len(before) != 2 || len(after) != 1 || after[0].AuthID != "genuine-oauth" || !after[0].ResetAt.Equal(before[1].ResetAt) {
		t.Fatalf("targeted unban changed genuine protection: before=%+v after=%+v", before, after)
	}
}
