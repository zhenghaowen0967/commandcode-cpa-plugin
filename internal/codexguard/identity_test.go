package codexguard

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRecordRequiresTrustedCodexOAuthIdentity(t *testing.T) {
	now := time.Date(2026, 10, 9, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, provider, authType, model, baseURL string
		want                                     bool
	}{
		{"real Codex", "codex", "oauth", "gpt-6-luna", "", true},
		{"OAuth behind gateway", "codex", "oauth", "glm-5.3", "https://gateway.example.test/v1", true},
		{"BigModel Flash", "codex", "apikey", "glm-5.3-flash", "https://open.bigmodel.cn/api/coding/paas/v4", false},
		{"BigModel GLM", "codex", "apikey", "glm-5.3", "https://open.bigmodel.cn/api/coding/paas/v4", false},
		{"API key with Codex model", "codex", "apikey", "gpt-6-luna", "", false},
		{"missing identity", "codex", "", "gpt-6-luna", "", false},
		{"unknown identity", "codex", "other", "gpt-6-luna", "", false},
		{"other provider", "openai", "oauth", "gpt-6-luna", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g Guard
			r := rateLimited("trusted-host-auth", nil)
			r.Provider, r.AuthType, r.Model, r.BaseURL = tc.provider, tc.authType, tc.model, tc.baseURL
			r.APIKey = "client-oauth-claim-is-not-upstream-identity"
			r.Failure.Body = `{"auth_type":"oauth","error":{"type":"rate_limit_exceeded"}}`
			if got := g.Record(r, now); got != tc.want {
				t.Fatalf("Record = %v, want %v", got, tc.want)
			}
			bans := g.Snapshot(now)
			if tc.want {
				if len(bans) != 1 || bans[0].Window != "fallback-5h" || !bans[0].ResetAt.Equal(now.Add(5*time.Hour)) {
					t.Fatalf("genuine OAuth fallback changed: %+v", bans)
				}
			} else if len(bans) != 0 {
				t.Fatalf("non-OAuth identity was held: %+v", bans)
			}
		})
	}
}

func TestAPIKeyHeadersCannotCreateCodexQuotaHold(t *testing.T) {
	now := time.Now()
	var g Guard
	r := rateLimited("static-key", futureFullHeader(now.Add(7*24*time.Hour)))
	r.AuthType = "apikey"
	if g.Record(r, now) || len(g.Snapshot(now)) != 0 {
		t.Fatal("quota-like headers bypassed the trusted credential identity")
	}
}

func TestFilterIgnoresHistoricalHoldOnlyForTrustedAPIKey(t *testing.T) {
	now := time.Now()
	var g Guard
	g.Import([]Ban{{AuthID: "same-id", ResetAt: now.Add(5 * time.Hour)}}, now)
	for _, tc := range []struct {
		name       string
		attributes map[string]string
		blocked    bool
	}{
		{"API key", map[string]string{"api_key": "public-synthetic-upstream-key"}, false},
		{"redacted API key", map[string]string{"auth_kind": "apikey"}, false},
		{"canonical API key kind", map[string]string{"auth_kind": "api_key"}, false},
		{"explicit OAuth overrides key", map[string]string{"auth_kind": "oauth", "api_key": "public-synthetic-upstream-key"}, true},
		{"OAuth", map[string]string{"base_url": "https://gateway.example.test/v1"}, true},
		{"unknown identity", nil, true},
		{"empty key", map[string]string{"api_key": ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := pluginapi.SchedulerAuthCandidate{ID: "same-id", Provider: "codex", Attributes: tc.attributes}
			available, blocked := g.Filter([]pluginapi.SchedulerAuthCandidate{candidate}, now)
			if blocked != tc.blocked || (len(available) == 0) != tc.blocked {
				t.Fatalf("available=%d blocked=%v", len(available), blocked)
			}
			if len(g.Snapshot(now)) != 1 {
				t.Fatal("filter silently cleared a hold instead of preserving targeted unban")
			}
		})
	}
}

func TestAPIKey429DoesNotExtendHistoricalHold(t *testing.T) {
	now := time.Now()
	var g Guard
	reset := now.Add(time.Minute)
	g.Import([]Ban{{AuthID: "static-key", ResetAt: reset}}, now)
	r := rateLimited("static-key", http.Header{"Retry-After": {"2"}})
	r.AuthType = "apikey"
	if g.Record(r, now) || !g.Snapshot(now)[0].ResetAt.Equal(reset) {
		t.Fatal("third-party rate limit extended a Codex hold")
	}
}
