package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/codexguard"
	"commandcode-cpa-plugin/resources"
)

func holdCodex(t *testing.T, m *Manager, id string) {
	t.Helper()
	r := pluginapi.UsageRecord{Provider: "codex", AuthType: "oauth", AuthID: id, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429, Body: "must-not-store-this-body"}, APIKey: "must-not-store-client-key"}
	var got map[string]any
	decodeResult(t, mustHandle(t, m, pluginabi.MethodUsageHandle, poolHookBody(t, r)), &got)
}

func unifiedPick(t *testing.T, m *Manager, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	r := pluginapi.SchedulerPickRequest{Candidates: candidates, Model: "shared", Options: pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {"unified-host-request"}}}}
	var got pluginapi.SchedulerPickResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, r)), &got)
	return got
}

func TestCodexUsageFiltersAndAllBannedRejectsWithoutBuiltinFallback(t *testing.T) {
	m := NewManager(nil)
	bad := pluginapi.SchedulerAuthCandidate{ID: "codex-bad", Provider: "codex", Priority: 100}
	good := pluginapi.SchedulerAuthCandidate{ID: "codex-good", Provider: "codex", Priority: 1}
	if got := unifiedPick(t, m, bad, good); got.Handled || got.Reject {
		t.Fatalf("unaffected Codex did not use configured builtin: %+v", got)
	}
	holdCodex(t, m, bad.ID)
	if got := unifiedPick(t, m, bad, good); !got.Handled || got.Reject || got.AuthID != good.ID || got.DelegateBuiltin != "" {
		t.Fatalf("held credential escaped filter: %+v", got)
	}
	holdCodex(t, m, good.ID)
	if got := unifiedPick(t, m, bad, good); !got.Handled || !got.Reject || got.AuthID != "" || got.DelegateBuiltin != "" {
		t.Fatalf("all held fell through to original candidates: %+v", got)
	}
	other := pluginapi.SchedulerAuthCandidate{ID: "external", Provider: "other", Priority: -1}
	if got := unifiedPick(t, m, bad, good, other); got.Reject || got.AuthID != other.ID {
		t.Fatalf("swallowed available unrelated provider: %+v", got)
	}
	if got := unifiedPick(t, m, other); got.Handled || got.Reject {
		t.Fatalf("claimed unrelated route without a filtered candidate: %+v", got)
	}
}

func TestUnifiedCodexFilterRetainsPoolQuotaAndMixedPriorities(t *testing.T) {
	m := newPoolHandlerManager(t)
	one := addPoolHandlerAccount(t, m, "unified-one-test-key", "One", "one")
	two := addPoolHandlerAccount(t, m, "unified-two-test-key", "Two", "two")
	makePoolHandlerEligible(t, m, one, .2, 100)
	makePoolHandlerEligible(t, m, two, .8, 10)
	bad := pluginapi.SchedulerAuthCandidate{ID: "held-codex", Provider: "codex", Priority: 1000}
	a := pluginapi.SchedulerAuthCandidate{ID: one.AuthID, Provider: ProviderID, Priority: 10}
	b := pluginapi.SchedulerAuthCandidate{ID: two.AuthID, Provider: ProviderID, Priority: 10}
	holdCodex(t, m, bad.ID)
	if got := unifiedPick(t, m, bad, a, b); got.Reject || got.AuthID != two.AuthID {
		t.Fatalf("quota pick lost after Codex exclusion: %+v", got)
	}
	for _, account := range m.pool.Snapshot() {
		if account.Inflight != 0 {
			t.Fatal("scheduler pre-acquired pool lease")
		}
	}
	other := pluginapi.SchedulerAuthCandidate{ID: "external", Provider: "other", Priority: 20}
	if got := unifiedPick(t, m, bad, a, b, other); got.AuthID != other.ID {
		t.Fatalf("ignored mixed-provider priority: %+v", got)
	}
	other.Priority = 1
	if got := unifiedPick(t, m, bad, a, b, other); got.AuthID != two.AuthID {
		t.Fatalf("lost highest eligible pool tier: %+v", got)
	}
	makePoolHandlerEligible(t, m, one, 0, 0)
	makePoolHandlerEligible(t, m, two, 0, 0)
	if got := unifiedPick(t, m, bad, a, b, other); got.Reject || got.AuthID != other.ID {
		t.Fatalf("unavailable pool swallowed mixed fallback: %+v", got)
	}
	if got := unifiedPick(t, m, bad, a, b); !got.Reject || got.AuthID != "" {
		t.Fatalf("ineligible pool bypassed: %+v", got)
	}
}

func TestCodexManagementUsesSameStateAsSchedulerAndNoSecrets(t *testing.T) {
	m := NewManager(nil)
	holdCodex(t, m, "codex-auth")
	resp := callPoolManagement(t, m, http.MethodGet, "/codex/bans", "")
	var got struct {
		Bans        []codexguard.Ban `json:"bans"`
		Scope       string           `json:"scope"`
		State       string           `json:"state"`
		Independent bool             `json:"host_cooldown_independent"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(resp.Body, &got) != nil || len(got.Bans) != 1 || !got.Independent || got.Scope != "single_process" || got.State != "memory_only" {
		t.Fatalf("list = %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "must-not-store") {
		t.Fatal("usage secrets leaked")
	}
	resp = callPoolManagement(t, m, http.MethodPost, "/codex/unban", `{"auth_id":"codex-auth"}`)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), `"cleared":true`) {
		t.Fatalf("unban = %s", resp.Body)
	}
	if got := unifiedPick(t, m, pluginapi.SchedulerAuthCandidate{ID: "codex-auth", Provider: "codex"}); got.Handled {
		t.Fatalf("management cleared a different ban state: %+v", got)
	}
	holdCodex(t, m, "a")
	holdCodex(t, m, "b")
	resp = callPoolManagement(t, m, http.MethodPost, "/codex/unban-all", `{}`)
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), `"cleared_count":2`) || !strings.Contains(string(resp.Body), `"bans":[]`) {
		t.Fatalf("all = %s", resp.Body)
	}
	for _, body := range []string{`{}`, `{"auth_id":""}`, `{"auth_id":" padded"}`, `{"auth_id":"x","unexpected":true}`, `{"auth_id":"x"} {}`} {
		if r := callPoolManagement(t, m, http.MethodPost, "/codex/unban", body); r.StatusCode != 400 {
			t.Fatalf("accepted invalid body %s", body)
		}
	}
	if r := callPoolManagement(t, m, http.MethodPost, "/codex/unban-all", `{"auth_id":"x"}`); r.StatusCode != 400 {
		t.Fatal("accepted unknown clear-all field")
	}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/codex"})
	if err != nil || resp.StatusCode != 200 || string(resp.Body) != resources.CodexPage {
		t.Fatal("Codex native resource unavailable")
	}
	m.execClosing.Store(true)
	if r := callPoolManagement(t, m, http.MethodGet, "/codex/bans", ""); r.StatusCode != 503 {
		t.Fatal("management ignored shutdown")
	}
}

func TestCodexHoldImportManagementMergesAndFilters(t *testing.T) {
	m := NewManager(nil)
	now := time.Now().UTC()
	longReset := now.Add(4 * time.Hour).Format(time.RFC3339Nano)
	shortReset := now.Add(2 * time.Hour).Format(time.RFC3339Nano)

	resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[{"auth_id":"migrated-auth","reset_at":"`+longReset+`"}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"imported_count":1`) {
		t.Fatalf("initial import = %d %s", resp.StatusCode, resp.Body)
	}
	bans := m.codex.Snapshot(time.Now())
	if len(bans) != 1 || bans[0].AuthID != "migrated-auth" || bans[0].Window != "imported" || !bans[0].ResetAt.Equal(now.Add(4*time.Hour)) {
		t.Fatalf("imported bans = %#v", bans)
	}
	if got := unifiedPick(t, m, pluginapi.SchedulerAuthCandidate{ID: "migrated-auth", Provider: "codex"}); !got.Handled || !got.Reject {
		t.Fatalf("imported hold did not filter scheduler auth: %+v", got)
	}

	resp = callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[{"auth_id":"migrated-auth","reset_at":"`+shortReset+`"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("short duplicate import = %d %s", resp.StatusCode, resp.Body)
	}
	if got := m.codex.Snapshot(time.Now())[0].ResetAt; !got.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("duplicate import shortened reset to %s", got)
	}

	// The old plugin's GET /bans metadata is accepted, but the imported
	// window label remains generic and only auth_id/reset_at affect state.
	legacyReset := now.Add(5 * time.Hour).Format(time.RFC3339Nano)
	legacy := `{"plugin":"codex-429-autoban","version":"0.2.2","count":1,"bans":[{"auth_id":"legacy-auth","window":"week","banned_at":"2026-10-08T10:00:00Z","banned_at_unix":1791453600,"reset_at":"` + legacyReset + `","reset_at_unix":` + strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10) + `,"remaining_seconds":18000}]}`
	resp = callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", legacy)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy response import = %d %s", resp.StatusCode, resp.Body)
	}
	bans = m.codex.Snapshot(time.Now())
	if len(bans) != 2 || bans[0].AuthID != "legacy-auth" || bans[0].Window != "imported" {
		t.Fatalf("legacy imported bans = %#v", bans)
	}
}

func TestCodexHoldImportRejectsInvalidBatchWithoutPartialMutation(t *testing.T) {
	m := NewManager(nil)
	future := time.Now().UTC().Add(4 * time.Hour).Format(time.RFC3339Nano)
	body := `{"bans":[{"auth_id":"would-be-partial","reset_at":"` + future + `"},{"auth_id":"bad","reset_at":"not-a-date"}]}`
	resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", body)
	if resp.StatusCode != http.StatusBadRequest || len(m.codex.Snapshot(time.Now())) != 0 {
		t.Fatalf("invalid batch = %d %s; bans=%#v", resp.StatusCode, resp.Body, m.codex.Snapshot(time.Now()))
	}
}

func TestCodexHoldImportStrictJSONClosingAndEmptyList(t *testing.T) {
	m := NewManager(nil)
	for _, body := range []string{
		`{}`,
		`{"bans":null}`,
		`{"bans":[],"unexpected":true}`,
		`{"bans":[{"auth_id":"x","reset_at":"2030-01-01T00:00:00Z","unexpected":true}]}`,
		`{"bans":[{"auth_id":"x","reset_at":1.5}]}`,
		`{"bans":[{"auth_id":"x","reset_at":253402300800}]}`,
		`{"bans":[{"auth_id":"bad\u0001id","reset_at":"2030-01-01T00:00:00Z"}]}`,
		`{"bans":[]} {}`,
	} {
		if resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("accepted invalid import body %s: %d %s", body, resp.StatusCode, resp.Body)
		}
	}
	expired := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[{"auth_id":"expired-import","reset_at":"`+expired+`"}]}`)
	if resp.StatusCode != http.StatusOK || len(m.codex.Snapshot(time.Now())) != 0 {
		t.Fatalf("expired import = %d %s; bans=%#v", resp.StatusCode, resp.Body, m.codex.Snapshot(time.Now()))
	}
	unixReset := time.Now().Add(4 * time.Hour).Unix()
	resp = callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[{"auth_id":"unix-import","reset_at":`+strconv.FormatInt(unixReset, 10)+`}]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"imported_count":1`) {
		t.Fatalf("Unix reset import = %d %s", resp.StatusCode, resp.Body)
	}
	if got := unifiedPick(t, m, pluginapi.SchedulerAuthCandidate{ID: "unix-import", Provider: "codex"}); !got.Handled || !got.Reject {
		t.Fatalf("Unix-imported hold did not filter scheduler auth: %+v", got)
	}

	if resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", strings.Repeat(" ", poolManagementBodyLimit+1)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized import = %d %s", resp.StatusCode, resp.Body)
	}
	m.codex.ClearAll()
	resp = callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[]}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"imported_count":0`) || !strings.Contains(string(resp.Body), `"bans":[]`) {
		t.Fatalf("empty import = %d %s", resp.StatusCode, resp.Body)
	}
	m.execClosing.Store(true)
	if resp := callPoolManagement(t, m, http.MethodPost, "/codex/bans/import", `{"bans":[]}`); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closing import = %d %s, want 503", resp.StatusCode, resp.Body)
	}
}

func TestCodexHoldExpiresAndSurvivesReconfigure(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	holdCodex(t, m, "live")
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, testValidYAML))
	if len(m.codex.Snapshot(time.Now())) != 1 {
		t.Fatal("reconfigure dropped active hold")
	}
	m.codex.ClearAll()
	m.codex.Record(pluginapi.UsageRecord{Provider: "codex", AuthType: "oauth", AuthID: "expired", Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 429}}, time.Now().Add(-6*time.Hour))
	if got := unifiedPick(t, m, pluginapi.SchedulerAuthCandidate{ID: "expired", Provider: "codex"}); got.Handled {
		t.Fatal("expired hold remained active")
	}
	if len(NewManager(nil).codex.Snapshot(time.Now())) != 0 {
		t.Fatal("new manager inherited memory-only holds")
	}
}
