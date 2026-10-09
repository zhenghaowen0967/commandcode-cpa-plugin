package plugin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

const legacyAliasYAML = `
model-aliases:
  cc-deepseek-v4.1-flash: deepseek/deepseek-v4.1-flash
  cc-deepseek-v4.1-flash-fast: deepseek/deepseek-v4.1-flash-fast
  deepseek/deepseek-v4.1-flash: deepseek/deepseek-v4.1-flash
  deepseek/deepseek-v4.1-flash-fast: deepseek/deepseek-v4.1-flash-fast
pool:
  max-concurrency: 1
`

func newLegacyAliasManager(t *testing.T) (*Manager, *mockCommandCode) {
	t.Helper()
	st := newMockCommandCode(t)
	st.mu.Lock()
	st.catalogBody = `{"data":[{"id":"deepseek/deepseek-v4.1-flash"},{"id":"deepseek/deepseek-v4.1-flash-fast"}]}`
	st.chatBody = `{"id":"legacy-completion","object":"chat.completion","model":"deepseek/deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	st.mu.Unlock()
	f := &fakeCaller{responder: newForwardingCaller()}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := fmt.Sprintf("base-url: %s/v1\nallow-http: true\napi-keys:\n  - value: sk-test-1\n  - value: sk-test-2\n%s", st.srv.URL, legacyAliasYAML)
	if env := decodeEnv(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, yamlText))); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	m.lifeMu.Lock()
	if m.poolQuotaStop != nil {
		m.poolQuotaStop()
		m.poolQuotaStop = nil
	}
	m.lifeMu.Unlock()
	seedIntegrationQuota(t, m)
	return m, st
}

func legacyRequestBody(t *testing.T, model, format string) []byte {
	t.Helper()
	body := map[string]any{"model": model}
	switch format {
	case "openai", "claude":
		body["messages"] = []map[string]string{{"role": "user", "content": "ok"}}
		body["max_tokens"] = 16
	case "openai-response":
		body["input"] = "ok"
	default:
		t.Fatalf("unexpected fixture format %q", format)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestLegacyAliasesPublishAndExecuteThroughPool(t *testing.T) {
	m, st := newLegacyAliasManager(t)
	variants := map[string]string{
		"cc-deepseek-v4.1-flash":                        "deepseek/deepseek-v4.1-flash",
		"cc-deepseek-v4.1-flash-fast":                   "deepseek/deepseek-v4.1-flash-fast",
		"deepseek/deepseek-v4.1-flash":                  "deepseek/deepseek-v4.1-flash",
		"deepseek/deepseek-v4.1-flash-fast":             "deepseek/deepseek-v4.1-flash-fast",
		"commandcode/deepseek/deepseek-v4.1-flash":      "deepseek/deepseek-v4.1-flash",
		"commandcode/deepseek/deepseek-v4.1-flash-fast": "deepseek/deepseek-v4.1-flash-fast",
	}
	models := integrationStaticIDs(t, m)
	if len(models) != len(variants) {
		t.Fatalf("published models = %v", models)
	}
	for requested, canonical := range variants {
		if models[requested].OwnedBy != ProviderID {
			t.Fatalf("model %q not published by the pool: %+v", requested, models[requested])
		}
		for _, format := range []string{"openai", "claude", "openai-response"} {
			t.Run(requested+"/"+format, func(t *testing.T) {
				before := len(m.pool.RequestEvents(0))
				env := mustExecute(t, m, requested, format, legacyRequestBody(t, requested, format))
				if !env.OK {
					t.Fatalf("execute: %+v", env.Error)
				}
				_, _, body := st.chatRequest(t)
				if body["model"] != canonical {
					t.Fatalf("upstream model = %v, want %q", body["model"], canonical)
				}
				events := m.pool.RequestEvents(0)[before:]
				if len(events) != 2 || events[0].Action != "acquire" || events[1].Action != "settle" || events[1].Reason != "upstream_complete" {
					t.Fatalf("execution did not acquire and settle: %+v", events)
				}
				if events[0].Model != requested || events[1].Model != requested || events[0].AttemptID != events[1].AttemptID || events[0].RequestID != events[1].RequestID {
					t.Fatalf("execution lost caller model or owner identity: %+v", events)
				}
				for _, account := range m.pool.Snapshot() {
					if account.Inflight != 0 {
						t.Fatalf("execution leaked group slot: %+v", account)
					}
				}
			})
		}
	}
	if len(m.pool.Snapshot()) != 2 {
		t.Fatal("model aliases created extra accounts")
	}
}

func TestLegacyAliasesShareGroupCapacityAcrossNamesAndKeys(t *testing.T) {
	m, st := newLegacyAliasManager(t)
	accounts := m.pool.Snapshot()
	if len(accounts) != 2 || accounts[0].GroupID != accounts[1].GroupID || accounts[0].MaxConcurrency != 1 || accounts[1].MaxConcurrency != 1 {
		t.Fatalf("fixture is not two keys sharing cap1: %+v", accounts)
	}
	lease, err := m.pool.Acquire(accounts[0].AuthID, "legacy-held-owner", "cc-deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Settle("owner_finished")
	for _, model := range []string{"cc-deepseek-v4.1-flash-fast", "commandcode/deepseek/deepseek-v4.1-flash", "deepseek/deepseek-v4.1-flash"} {
		request := pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: model,
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: accounts[0].AuthID, Provider: ProviderID}, {ID: accounts[1].AuthID, Provider: ProviderID}},
			Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {"legacy-cap-" + model}}},
		}
		var pick pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &pick)
		if !pick.Handled || !pick.Reject || pick.AuthID != "" {
			t.Fatalf("full shared group bypassed by %q: %+v", model, pick)
		}
		env := decodeEnv(t, mustHandle(t, m, "executor.execute", execReqBodyWithKey(model, "openai", legacyRequestBody(t, model, "openai"), false, "sk-test-2")))
		if env.OK {
			t.Fatalf("second key bypassed shared capacity using %q", model)
		}
	}
	if hits, _ := st.chatCalls(); hits != 0 {
		t.Fatalf("capacity rejection reached upstream %d times", hits)
	}
	for _, account := range m.pool.Snapshot() {
		if account.Inflight != 1 {
			t.Fatalf("rejected request released another owner: %+v", account)
		}
	}
	lease.Settle("upstream_complete")
	if env := mustExecute(t, m, "cc-deepseek-v4.1-flash-fast", "openai", legacyRequestBody(t, "cc-deepseek-v4.1-flash-fast", "openai")); !env.OK {
		t.Fatalf("confirmed cleanup did not permit legacy alias: %+v", env.Error)
	}
}

func TestLegacyAliasesUseQuotaSelectionAndRejectWithoutFallback(t *testing.T) {
	m, st := newLegacyAliasManager(t)
	accounts := m.pool.Snapshot()
	makePoolHandlerEligible(t, m, accounts[0], 0.2, 100)
	makePoolHandlerEligible(t, m, accounts[1], 0.8, 10)
	models := []string{"cc-deepseek-v4.1-flash", "cc-deepseek-v4.1-flash-fast", "commandcode/deepseek/deepseek-v4.1-flash"}
	for i, model := range models {
		requestID := fmt.Sprintf("legacy-quota-%d", i)
		request := pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: model,
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: accounts[0].AuthID, Provider: ProviderID}, {ID: accounts[1].AuthID, Provider: ProviderID}},
			Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {requestID}}},
		}
		var pick pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &pick)
		if !pick.Handled || pick.Reject || pick.AuthID != accounts[1].AuthID {
			t.Fatalf("legacy alias ignored window headroom: %+v", pick)
		}
		credential, ok := m.pool.Credential(pick.AuthID)
		if !ok {
			t.Fatal("selected auth not in pool")
		}
		execBody := execReqBodyWithKey(model, "openai", legacyRequestBody(t, model, "openai"), false, credential.APIKey)
		var execReq executorRequest
		if err := json.Unmarshal(execBody, &execReq); err != nil {
			t.Fatal(err)
		}
		execReq.Headers.Set(requestIDHeader, requestID)
		env := decodeEnv(t, mustHandle(t, m, "executor.execute", poolHookBody(t, execReq)))
		if !env.OK {
			t.Fatalf("selected legacy execution failed: %+v", env.Error)
		}
		_, auths := st.chatCalls()
		if auths[len(auths)-1] != "Bearer "+credential.APIKey {
			t.Fatal("legacy execution used a different account than the quota-selected auth")
		}
	}
	beforeHits, _ := st.chatCalls()
	for _, account := range accounts {
		if err := m.pool.ObserveQuota(account.ID, pool.Quota{Headroom: 0, RemainingCredits: 0, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, model := range models {
		request := pluginapi.SchedulerPickRequest{
			Provider: ProviderID, Model: model,
			Candidates: []pluginapi.SchedulerAuthCandidate{{ID: accounts[0].AuthID, Provider: ProviderID}, {ID: accounts[1].AuthID, Provider: ProviderID}},
			Options:    pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {"legacy-empty-" + model}}},
		}
		var pick pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &pick)
		if !pick.Handled || !pick.Reject || pick.RejectCode != "pool_unavailable" {
			t.Fatalf("legacy alias fell back with unavailable quota: %+v", pick)
		}
		if env := mustExecute(t, m, model, "openai", legacyRequestBody(t, model, "openai")); env.OK {
			t.Fatalf("legacy alias %q bypassed execution quota admission", model)
		}
	}
	if hits, _ := st.chatCalls(); hits != beforeHits {
		t.Fatalf("rejected legacy requests reached upstream: before=%d, after=%d", beforeHits, hits)
	}
}
