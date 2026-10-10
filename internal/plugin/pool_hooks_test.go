package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

func poolHookBody(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func makePoolHandlerEligible(t *testing.T, m *Manager, account pool.AccountView, headroom, credits float64) {
	t.Helper()
	if err := m.pool.ObserveQuota(account.ID, pool.Quota{Headroom: headroom, RemainingCredits: credits, UpdatedAt: time.Now(), Windows: []pool.Window{}}); err != nil {
		t.Fatal(err)
	}
}

func TestPoolSchedulerUsesActualCandidatesAndDoesNotAcquire(t *testing.T) {
	m := newPoolHandlerManager(t)
	one := addPoolHandlerAccount(t, m, "hook-scheduler-one", "One", "one")
	two := addPoolHandlerAccount(t, m, "hook-scheduler-two", "Two", "two")
	makePoolHandlerEligible(t, m, one, 0.2, 100)
	makePoolHandlerEligible(t, m, two, 0.8, 10)
	request := pluginapi.SchedulerPickRequest{Provider: ProviderID, Model: "any-model", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: one.AuthID, Provider: ProviderID}, {ID: two.AuthID, Provider: ProviderID}}, Options: pluginapi.SchedulerOptions{Headers: map[string][]string{requestIDHeader: {"host-pick-request"}}}}
	var got pluginapi.SchedulerPickResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
	if !got.Handled || got.Reject || got.AuthID != two.AuthID {
		t.Fatalf("pick = %+v", got)
	}
	for _, account := range m.pool.Snapshot() {
		if account.Inflight != 0 {
			t.Fatalf("scheduler pre-acquired: %+v", account)
		}
	}
	events := m.pool.Events(0)
	if len(events) == 0 || events[len(events)-1].RequestID != "host-pick-request" {
		t.Fatalf("pick did not correlate host identity: %+v", events)
	}
	request.Candidates = request.Candidates[:1]
	decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
	if got.AuthID != one.AuthID {
		t.Fatalf("picked account absent from host candidates: %+v", got)
	}
	request.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: two.AuthID, Provider: "other"}}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
	if got.Handled || got.Reject || got.AuthID != "" {
		t.Fatalf("claimed mismatched actual provider: %+v", got)
	}
}

func TestPoolSchedulerRejectsPoolOnlyAndLeavesMixedAndOtherModels(t *testing.T) {
	m := newPoolHandlerManager(t)
	account := addPoolHandlerAccount(t, m, "hook-scheduler-stale", "Stale", "g")
	for _, request := range []pluginapi.SchedulerPickRequest{
		{Provider: ProviderID, Model: "plain", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: ProviderID}}},
		{Provider: ProviderID, Model: "plain"},
		{Providers: []string{ProviderID}, Model: "plain"},
	} {
		var got pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
		if !got.Handled || !got.Reject || got.AuthID != "" || got.RejectCode == "" {
			t.Fatalf("pool fell back: %+v", got)
		}
	}
	for _, request := range []pluginapi.SchedulerPickRequest{
		{Provider: "other", Model: "commandcode/shared", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "external", Provider: "other"}}},
		{Provider: "other", Model: "commandcode/shared"},
		{Model: "commandcode/shared"},
		{Providers: []string{ProviderID, "other"}, Model: "shared", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: account.AuthID, Provider: ProviderID}, {ID: "external", Provider: "other"}}},
	} {
		var got pluginapi.SchedulerPickResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, request)), &got)
		if got.Handled || got.Reject {
			t.Fatalf("swallowed unrelated/mixed route: %+v", got)
		}
	}
}

func TestPoolInterceptBeforeOverwritesSpoofAndAfterOnlyTrustedSelectedAuth(t *testing.T) {
	m := newPoolHandlerManager(t)
	account := addPoolHandlerAccount(t, m, "hook-intercept-secret", "Pool", "g")
	req := pluginapi.RequestInterceptRequest{RequestID: "trusted-host-id", Headers: http.Header{requestIDHeader: {"client-spoof"}, strings.ToLower(requestIDHeader): {"another-spoof"}}, Metadata: map[string]any{"request_id": "metadata-spoof", "auth_provider": ProviderID}}
	var got pluginapi.RequestInterceptResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptBefore, poolHookBody(t, req)), &got)
	if len(got.ClearHeaders) != 2 || got.ClearHeaders[0] != requestIDHeader || poolHookRequestID(got.Headers) != req.RequestID {
		t.Fatalf("before = %+v", got)
	}
	if poolHookRequestID(got.Headers) == "client-spoof" {
		t.Fatal("client ID preserved")
	}
	for _, selected := range []string{"", "external", "unknown-pool-id"} {
		req.Metadata[selectedPoolAuthMetadataKey] = selected
		req.Model, req.RequestedModel = "commandcode/same-model", "commandcode/same-model"
		decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptAfter, poolHookBody(t, req)), &got)
		if got.Terminate || len(got.Headers) != 0 || len(got.ClearHeaders) != 2 {
			t.Fatalf("claimed non-pool auth via prefix/metadata: %+v", got)
		}
	}
	m.cfg.ModelPrefix.Enabled = false
	req.Model, req.RequestedModel = "plain-model", "plain-model"
	req.Metadata[selectedPoolAuthMetadataKey] = account.AuthID
	decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptAfter, poolHookBody(t, req)), &got)
	if got.Terminate || poolHookRequestID(got.Headers) != "trusted-host-id" || len(got.ClearHeaders) != 2 {
		t.Fatalf("unprefixed trusted pool = %+v", got)
	}
	if len(req.Headers) != 2 {
		t.Fatal("mutated read-only request headers")
	}
}

func TestPoolInterceptMissingHostIDFailsClosedWithoutMetadataFallback(t *testing.T) {
	m := newPoolHandlerManager(t)
	account := addPoolHandlerAccount(t, m, "hook-missing-secret", "Pool", "g")
	for _, id := range []string{"", " ", " padded", "id\r\ninjected"} {
		req := pluginapi.RequestInterceptRequest{RequestID: id, TraceID: "trace-is-not-id", Metadata: map[string]any{selectedPoolAuthMetadataKey: account.AuthID, "request_id": "metadata-fake"}, Headers: http.Header{requestIDHeader: {"client-fake"}}}
		var got pluginapi.RequestInterceptResponse
		decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptAfter, poolHookBody(t, req)), &got)
		if !got.Terminate || got.StatusCode != http.StatusServiceUnavailable || len(got.Headers) != 0 {
			t.Fatalf("missing host ID accepted: %+v", got)
		}
		if strings.Contains(string(got.ResponseBody), "fake") || strings.Contains(string(got.ResponseBody), "hook-missing-secret") {
			t.Fatalf("unsafe termination: %s", got.ResponseBody)
		}
	}
}

func TestPoolRequestCompleteCancelsWithoutSettling(t *testing.T) {
	m := newPoolHandlerManager(t)
	account := addPoolHandlerAccount(t, m, "hook-complete-secret", "Pool", "g")
	makePoolHandlerEligible(t, m, account, 0.9, 100)
	lease, err := m.pool.Acquire(account.AuthID, "host-completion", "model")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Settle("test_cleanup") })
	mustHandle(t, m, pluginabi.MethodRequestComplete, poolHookBody(t, pluginapi.RequestCompletion{RequestID: "host-completion", Outcome: pluginapi.RequestCompletionCanceled, Metadata: map[string]any{"request_id": "different"}}))
	select {
	case <-lease.Context.Done():
	default:
		t.Fatal("completion did not cancel lease")
	}
	if m.pool.Snapshot()[0].Inflight != 1 {
		t.Fatal("completion released slot before upstream cleanup")
	}
	if _, err := m.pool.Acquire(account.AuthID, "host-completion", "model"); err == nil {
		t.Fatal("late attempt acquired after completion")
	}
	lease.Settle("test_cleanup")
	if m.pool.Snapshot()[0].Inflight != 0 {
		t.Fatal("execution cleanup did not release slot")
	}
}

func TestPoolHookMalformedRequestsAndAmbiguousPrivateHeaders(t *testing.T) {
	m := newPoolHandlerManager(t)
	for _, method := range []string{pluginabi.MethodSchedulerPick, pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter, pluginabi.MethodRequestComplete} {
		var envelope pluginabi.Envelope
		if json.Unmarshal(mustHandle(t, m, method, []byte(`{"broken"`)), &envelope) != nil || envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_request" {
			t.Fatalf("malformed request accepted by %s", method)
		}
	}
	for _, headers := range []map[string][]string{
		{requestIDHeader: {"one", "two"}},
		{requestIDHeader: {"one"}, strings.ToLower(requestIDHeader): {"two"}},
		{requestIDHeader: {" padded"}},
	} {
		if poolHookRequestID(headers) != "" {
			t.Fatalf("ambiguous identity accepted: %+v", headers)
		}
	}
}
