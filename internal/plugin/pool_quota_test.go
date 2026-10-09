package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/pool"
)

const poolQuotaTestCredits = `{"credits":{"monthlyCredits":12,"purchasedCredits":3,"freeCredits":2},"windowLimits":{"limited":false,"exceeded":null,"fiveHour":{"used":20,"cap":100,"exceeded":false,"resetAt":1791446400000},"weekly":{"used":80,"cap":100,"exceeded":false}}}`

func TestParsePoolCreditsSumAndMinimumWindows(t *testing.T) {
	quota, err := parsePoolCredits([]byte(poolQuotaTestCredits))
	if err != nil {
		t.Fatal(err)
	}
	if quota.RemainingCredits != 17 || math.Abs(quota.Headroom-0.2) > 1e-12 {
		t.Fatalf("quota=%+v, want 17 credits and minimum headroom 0.2", quota)
	}
	if quota.MonthlyCredits == nil || *quota.MonthlyCredits != 12 || quota.Month != nil {
		t.Fatalf("monthly balance must remain separate from purchased/free, without inventing a plan: %+v", quota)
	}
	if len(quota.Windows) != 2 || quota.Windows[0].Remaining != 80 || quota.Windows[1].Remaining != 20 {
		t.Fatalf("windows=%+v", quota.Windows)
	}
	if quota.Windows[0].ResetAt.UnixMilli() != 1791446400000 {
		t.Fatalf("reset=%v, want exact Unix milliseconds", quota.Windows[0].ResetAt)
	}
}

func TestParsePoolCreditsDynamicExceededAndLimited(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exceeded any
		limited  bool
		window   any
		blocked  bool
	}{
		{"null", nil, false, false, false},
		{"false", false, false, false, false},
		{"empty string", "", false, false, false},
		{"nonempty whitespace string", " ", false, false, true},
		{"named window", "weekly", false, false, true},
		{"true", true, false, false, true},
		{"limited with available windows", nil, true, false, false},
		{"limited and global exceeded", true, true, false, true},
		{"limited and window exceeded", nil, true, true, true},
		{"window exceeded", nil, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			_ = json.Unmarshal([]byte(poolQuotaTestCredits), &body)
			limits := body["windowLimits"].(map[string]any)
			limits["exceeded"], limits["limited"] = tc.exceeded, tc.limited
			limits["fiveHour"].(map[string]any)["exceeded"] = tc.window
			raw, _ := json.Marshal(body)
			quota, err := parsePoolCredits(raw)
			if err != nil {
				t.Fatal(err)
			}
			if (quota.Headroom == 0) != tc.blocked {
				t.Fatalf("headroom=%v blocked=%v", quota.Headroom, tc.blocked)
			}
			if !tc.blocked && math.Abs(quota.Headroom-0.2) > 1e-12 {
				t.Fatalf("available windows must retain measured headroom 0.2: %+v", quota)
			}
		})
	}
}

func TestParsePoolCreditsRejectsInvalidOrMissingMetrics(t *testing.T) {
	validNoWindows := `{"credits":{"monthlyCredits":0,"purchasedCredits":0,"freeCredits":0}}`
	for _, body := range []string{
		`{}`, `null`, `{"credits":null}`,
		`{"credits":{"monthlyCredits":1,"purchasedCredits":0}}`,
		`{"credits":{"monthlyCredits":null,"purchasedCredits":0,"freeCredits":0}}`,
		`{"credits":{"monthlyCredits":-1,"purchasedCredits":0,"freeCredits":0}}`,
		`{"credits":{"monthlyCredits":1e400,"purchasedCredits":0,"freeCredits":0}}`,
		`{"credits":{"monthlyCredits":1e308,"purchasedCredits":1e308,"freeCredits":0}}`,
		strings.Replace(poolQuotaTestCredits, `"used":20`, `"used":null`, 1),
		strings.Replace(poolQuotaTestCredits, `"used":20`, `"used":-1`, 1),
		strings.Replace(poolQuotaTestCredits, `"used":20,`, ``, 1),
		strings.Replace(poolQuotaTestCredits, `"cap":100`, `"cap":0`, 1),
		strings.Replace(poolQuotaTestCredits, `"cap":100`, `"cap":-1`, 1),
		strings.Replace(poolQuotaTestCredits, `"exceeded":null`, `"exceeded":{"credential":"synthetic-secret"}`, 1),
		validNoWindows + `{"extra":true}`,
	} {
		_, err := parsePoolCredits([]byte(body))
		if !errors.Is(err, errPoolQuotaInvalid) || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatalf("body=%q error=%v, want fixed invalid error", body, err)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if finiteNonnegative(value) {
			t.Fatalf("accepted nonfinite/negative metric %v", value)
		}
	}
	for _, body := range []string{validNoWindows, strings.TrimSuffix(validNoWindows, "}") + `,"windowLimits":{"limited":false}}`} {
		quota, err := parsePoolCredits([]byte(body))
		if err != nil || quota.Headroom != 0 || len(quota.Windows) != 0 {
			t.Fatalf("no metrics must not imply unlimited: quota=%+v err=%v", quota, err)
		}
	}
	quota, err := parsePoolCredits([]byte(strings.Replace(poolQuotaTestCredits, `"used":20`, `"used":200`, 1)))
	if err != nil || quota.Headroom != 0 || quota.Windows[0].Remaining != 0 {
		t.Fatalf("over-cap must clamp to zero: quota=%+v err=%v", quota, err)
	}
}

func newPoolQuotaTestManager(t *testing.T, base string, count int) (*Manager, []pool.AccountView) {
	t.Helper()
	p, err := pool.New(pool.Config{AuthDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "pool.state"), QuotaMaxAge: time.Minute, DefaultLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var accounts []pool.AccountView
	for i := range count {
		view, err := p.Upsert(pool.AccountInput{Name: fmt.Sprintf("quota-test-%d", i), GroupID: "quota-test-group", APIKey: fmt.Sprintf("synthetic-quota-key-%d", i), MaxConcurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, view)
	}
	return &Manager{pool: p, cfg: config.Config{BaseURL: base + "/provider/v1", RequestTimeout: time.Second}}, accounts
}

func poolQuotaByID(t *testing.T, m *Manager, id string) pool.Quota {
	t.Helper()
	for _, account := range m.pool.Snapshot() {
		if account.ID == id {
			return account.Quota
		}
	}
	t.Fatal("account disappeared")
	return pool.Quota{}
}

func TestRefreshPoolQuotasRealHTTPAndIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-quota-key-0" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "cli" {
			t.Error("native probe did not set expected bearer/accept/client headers")
		}
		switch r.URL.Path {
		case accountCreditsPath:
			_, _ = io.WriteString(w, poolQuotaTestCredits)
		case accountSubscriptionPath:
			_, _ = io.WriteString(w, `{"success":true,"data":{"planId":"individual-ultra"}}`)
		case "/alpha/whoami":
			if r.URL.Query().Get("limits") != "1" {
				t.Error("missing limits query")
			}
			_, _ = io.WriteString(w, `{"success":true,"org":{"id":"org-stable","login":"not-an-id"},"user":{"id":"user-stable","email":"test@example.invalid"}}`)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
	if err := m.refreshPoolQuotas(context.Background(), accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	quota := poolQuotaByID(t, m, accounts[0].ID)
	if quota.Identity != "org:org-stable" || quota.Email != "test@example.invalid" || quota.Plan != "Ultra" || quota.UpdatedAt.IsZero() {
		t.Fatalf("quota=%+v", quota)
	}
	if quota.RemainingCredits != 17 || math.Abs(quota.Headroom-0.2) > 1e-12 {
		t.Fatalf("display plan must not participate in routing quota: %+v", quota)
	}
}

func TestFetchPoolQuotaMonthlyDisplayPlanAllowance(t *testing.T) {
	for _, tc := range []struct {
		name, plan, wantPlan string
		monthly, wantCap     float64
	}{
		{"ultra", "individual-ultra", "Ultra", 12, 300},
		{"go", "individual-go", "Go", 4, 10},
		{"goat", "individual-goat", "GOAT", 12, 70},
		{"pro", "individual-pro", "Pro", 12, 30},
		{"pro v1", "individual-pro-v1", "Pro", 12, 80},
		{"provider", "individual-provider", "Provider", 12, 15},
		{"max", "individual-max", "Max", 12, 150},
		{"teams", "teams-pro", "Teams Pro", 12, 40},
		{"normalized prefix", " Individual_Pro_V1_Annual ", "Pro", 12, 80},
		{"zero monthly", "individual-ultra", "Ultra", 0, 300},
		{"above static allowance", "individual-ultra", "Ultra", 350, 350},
		{"unknown plan", " custom-plan ", "custom-plan", 12, 0},
		{"unknown zero", "custom-plan", "custom-plan", 0, 0},
		{"absent plan", "", "", 12, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credits := strings.Replace(poolQuotaTestCredits, `"monthlyCredits":12`, fmt.Sprintf(`"monthlyCredits":%v`, tc.monthly), 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case accountCreditsPath:
					_, _ = io.WriteString(w, credits)
				case accountSubscriptionPath:
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"planId": tc.plan, "status": " active ", "currentPeriodEnd": "2026-10-16T04:03:37.000Z"}})
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			quota, err := fetchPoolQuota(context.Background(), server.Client(), server.URL, time.Second, "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			if quota.MonthlyCredits == nil || *quota.MonthlyCredits != tc.monthly || quota.Plan != tc.wantPlan || quota.SubscriptionStatus != "active" {
				t.Fatalf("quota=%+v want plan=%q monthly=%v", quota, tc.wantPlan, tc.monthly)
			}
			if tc.wantCap == 0 {
				if quota.Month != nil {
					t.Fatalf("unknown plan invented denominator: %+v", quota.Month)
				}
			} else if quota.Month == nil || quota.Month.Name != "month" || quota.Month.Source != "plan_allowance" || quota.Month.Cap != tc.wantCap || quota.Month.Remaining != tc.monthly || quota.Month.Used != math.Max(0, tc.wantCap-tc.monthly) || !quota.Month.ResetAt.IsZero() {
				t.Fatalf("month=%+v want monthly-only remaining=%v cap=%v without assumed reset", quota.Month, tc.monthly, tc.wantCap)
			}
			if quota.RemainingCredits != tc.monthly+5 || math.Abs(quota.Headroom-0.2) > 1e-12 || len(quota.Windows) != 2 || quota.Windows[0].Name != "five_hour" || quota.Windows[1].Name != "weekly" {
				t.Fatalf("display month changed measured quota: %+v", quota)
			}
		})
	}
}

func TestFetchPoolQuotaSubscriptionPeriodEndNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"offset", `"2026-10-16T12:03:37+08:00"`, "2026-10-16T04:03:37Z"},
		{"milliseconds", `"2026-10-16T04:03:37.123Z"`, "2026-10-16T04:03:37Z"},
		{"date", `"2026-10-16"`, "2026-10-16T00:00:00Z"},
		{"no timezone", `"2026-10-16T04:03:37"`, "2026-10-16T04:03:37Z"},
		{"past", `"2000-01-01T00:00:00Z"`, "2000-01-01T00:00:00Z"},
		{"absent", "", ""},
		{"null", "null", ""},
		{"empty", `""`, ""},
		{"invalid", `"not-a-date"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dateField := ""
			if tc.raw != "" {
				dateField = `,"currentPeriodEnd":` + tc.raw
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case accountCreditsPath:
					_, _ = io.WriteString(w, poolQuotaTestCredits)
				case accountSubscriptionPath:
					_, _ = io.WriteString(w, `{"success":true,"data":{"planId":"individual-ultra","status":"active"`+dateField+`}}`)
				default:
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			quota, err := fetchPoolQuota(context.Background(), server.Client(), server.URL, time.Second, "synthetic-key")
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if quota.SubscriptionPeriodEnd != nil {
					t.Fatalf("invented period end: %v", quota.SubscriptionPeriodEnd)
				}
			} else if quota.SubscriptionPeriodEnd == nil || quota.SubscriptionPeriodEnd.Format(time.RFC3339) != tc.want || quota.SubscriptionPeriodEnd.Location() != time.UTC {
				t.Fatalf("period end=%v want %s UTC", quota.SubscriptionPeriodEnd, tc.want)
			}
			if quota.Month == nil || !quota.Month.ResetAt.IsZero() || quota.Headroom == 0 || quota.SubscriptionStatus != "active" {
				t.Fatalf("date changed monthly reset, routing, or subscription status: %+v", quota)
			}
		})
	}
}

func TestRefreshPoolQuotasUsesConfiguredProxyAndCLIUserAgent(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Scheme != "http" || r.URL.Host != "provider.invalid" {
			t.Errorf("quota probe did not use configured proxy target: %s", r.URL.Redacted())
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-quota-key-0" || r.Header.Get("User-Agent") != "cli" {
			t.Error("quota proxy request missing expected synthetic auth or cli user-agent")
		}
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case accountCreditsPath:
			_, _ = io.WriteString(w, poolQuotaTestCredits)
		case accountSubscriptionPath:
			_, _ = io.WriteString(w, `{"success":true,"data":{"planId":"individual-ultra"}}`)
		case "/alpha/whoami":
			_, _ = io.WriteString(w, `{"success":true,"org":{"id":"org-stable"},"user":{"id":"user-stable"}}`)
		default:
			t.Errorf("unexpected proxied endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer proxy.Close()
	m, accounts := newPoolQuotaTestManager(t, "http://provider.invalid", 1)
	m.cfg.ProxyURL = proxy.URL
	if err := m.refreshPoolQuotas(context.Background(), accounts[0].ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen[accountCreditsPath] != 1 || seen[accountSubscriptionPath] != 1 || seen["/alpha/whoami"] != 1 || len(seen) != 3 {
		t.Fatalf("proxied account GET paths = %v, want all three exactly once", seen)
	}
}

func TestFetchPoolQuotaIdentityOnlyActualStableIDs(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"org":{"login":"not-identity"},"user":{"email":"x@example.invalid","name":"a","userName":"b"}}`, ""},
		{`{"org":{"id":null},"user":{"id":"user-1"}}`, "user:user-1"},
		{`{"org":{"id":"org-1"},"user":{"id":"user-1"}}`, "org:org-1"},
		{`{"org":{"id":42},"user":{"id":"user-1"}}`, "org:42"},
		{`{"org":{"id":true},"user":{"id":"user-1"}}`, "user:user-1"},
		{`{"success":false,"org":{"id":"org-1"},"user":{"id":"user-1"}}`, ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == accountCreditsPath {
					_, _ = io.WriteString(w, poolQuotaTestCredits)
				} else if r.URL.Path == "/alpha/whoami" {
					_, _ = io.WriteString(w, tc.body)
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			quota, err := fetchPoolQuota(context.Background(), server.Client(), server.URL, time.Second, "synthetic-key")
			if err != nil || quota.Identity != tc.want || quota.RemainingCredits != 17 {
				t.Fatalf("quota=%+v err=%v want identity=%q", quota, err, tc.want)
			}
		})
	}
}

func TestRefreshPoolQuotasMetadataFailurePreservesCreditsAndKnownIdentity(t *testing.T) {
	for _, body := range []string{"HTTP failure", "invalid JSON", `{"success":false,"data":{"planId":"individual-ultra"}}`, `{"success":true,"data":{"planId":"individual-ultra","currentPeriodEnd":42}}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == accountCreditsPath {
					_, _ = io.WriteString(w, poolQuotaTestCredits)
				} else if body == "HTTP failure" || r.URL.Path != accountSubscriptionPath {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "synthetic-quota-key-0")
				} else {
					_, _ = io.WriteString(w, body)
				}
			}))
			defer server.Close()
			m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
			monthly := 99.0
			periodEnd := time.Now().UTC()
			if err := m.pool.ObserveQuota(accounts[0].ID, pool.Quota{Identity: "org:already-known", UpdatedAt: time.Now(), Plan: "old-plan", Email: "old@example.invalid",
				Month: &pool.Window{Name: "month", Source: "plan_allowance", Cap: 300, Remaining: 99}, MonthlyCredits: &monthly,
				SubscriptionPeriodEnd: &periodEnd, SubscriptionStatus: "old-status"}); err != nil {
				t.Fatal(err)
			}
			if err := m.refreshPoolQuotas(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			quota := poolQuotaByID(t, m, accounts[0].ID)
			if quota.Error != "" || quota.RemainingCredits != 17 || quota.Headroom == 0 || quota.Identity != "org:already-known" || quota.MonthlyCredits == nil || *quota.MonthlyCredits != 12 {
				t.Fatalf("metadata failure destroyed real quota or known identity: %+v", quota)
			}
			if quota.Month != nil || quota.SubscriptionPeriodEnd != nil || quota.SubscriptionStatus != "" || quota.Plan != "" || quota.Email != "" {
				t.Fatalf("metadata failure claimed stale display data was fresh: %+v", quota)
			}
		})
	}
}

func TestPoolQuotaDisplayJSONEndToEnd(t *testing.T) {
	for _, tc := range []struct {
		name, plan, status, date, wantStatus string
		wantMonth                            bool
	}{
		{"known plan", "individual-ultra", "active", "2026-10-16T12:03:37+08:00", "active", true},
		{"unknown plan", "custom-plan", "", "", "", false},
		{"secret status", "individual-ultra", "synthetic-quota-key-0", "", "[redacted]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case accountCreditsPath:
					_, _ = io.WriteString(w, strings.Replace(poolQuotaTestCredits, `"monthlyCredits":12`, `"monthlyCredits":0`, 1))
				case accountSubscriptionPath:
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"planId": tc.plan, "status": tc.status, "currentPeriodEnd": tc.date}})
				case "/alpha/whoami":
					_, _ = io.WriteString(w, `{"success":true,"org":{"id":"json-org"},"user":{"email":"json@example.invalid"}}`)
				default:
					t.Errorf("unexpected test endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
			for _, endpoint := range []string{"/quota/refresh", "/accounts"} {
				method, body := http.MethodGet, ""
				if endpoint == "/quota/refresh" {
					method, body = http.MethodPost, `{}`
				}
				resp := callPoolManagement(t, m, method, endpoint, body)
				if resp.StatusCode != http.StatusOK || strings.Contains(string(resp.Body), "synthetic-quota-key-0") {
					t.Fatalf("management response failed or leaked secret: %d %s", resp.StatusCode, resp.Body)
				}
				var decoded struct {
					Accounts []struct {
						Quota map[string]json.RawMessage `json:"quota"`
					} `json:"accounts"`
				}
				if err := json.Unmarshal(resp.Body, &decoded); err != nil || len(decoded.Accounts) != 1 {
					t.Fatalf("invalid management JSON: %s err=%v", resp.Body, err)
				}
				q := decoded.Accounts[0].Quota
				if string(q["monthly_credits"]) != "0" || string(q["remaining_credits"]) != "5" {
					t.Fatalf("zero monthly must be present and exclude purchased/free: %s", resp.Body)
				}
				monthRaw, hasMonth := q["month"]
				if hasMonth != tc.wantMonth {
					t.Fatalf("month presence=%v want=%v: %s", hasMonth, tc.wantMonth, resp.Body)
				}
				if hasMonth {
					var month pool.Window
					if err := json.Unmarshal(monthRaw, &month); err != nil || month.Name != "month" || month.Source != "plan_allowance" || month.Cap != 300 || month.Used != 300 || month.Remaining != 0 || !month.ResetAt.IsZero() {
						t.Fatalf("invalid monthly display JSON: %s err=%v", monthRaw, err)
					}
				}
				if statusRaw, exists := q["subscription_status"]; tc.wantStatus == "" {
					if exists {
						t.Fatalf("absent status not omitted: %s", resp.Body)
					}
				} else {
					var status string
					if err := json.Unmarshal(statusRaw, &status); err != nil || status != tc.wantStatus {
						t.Fatalf("status=%q want=%q err=%v", status, tc.wantStatus, err)
					}
				}
				if dateRaw, exists := q["subscription_period_end"]; tc.date == "" {
					if exists {
						t.Fatalf("absent date not omitted: %s", resp.Body)
					}
				} else if string(dateRaw) != `"2026-10-16T04:03:37Z"` {
					t.Fatalf("date not normalized: %s", dateRaw)
				}
				var windows []pool.Window
				if err := json.Unmarshal(q["windows"], &windows); err != nil || len(windows) != 2 || windows[0].Name != "five_hour" || windows[1].Name != "weekly" || strings.Contains(string(q["windows"]), `"source"`) {
					t.Fatalf("monthly display mixed into existing windows or empty source not omitted: %s err=%v", q["windows"], err)
				}
				var headroom float64
				if err := json.Unmarshal(q["headroom"], &headroom); err != nil || math.Abs(headroom-0.2) > 1e-12 {
					t.Fatalf("management display changed routing headroom: %v err=%v", headroom, err)
				}
			}
			decision := m.pool.Pick([]string{accounts[0].AuthID}, "json-display", "model")
			if decision.AuthID != accounts[0].AuthID {
				t.Fatalf("zero monthly balance changed eligibility: %+v", decision)
			}
		})
	}
}

func TestRefreshPoolQuotasFailedSnapshotOnlyAffectedAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer synthetic-quota-key-0" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "synthetic-quota-key-0")
			return
		}
		if r.URL.Path == accountCreditsPath {
			_, _ = io.WriteString(w, poolQuotaTestCredits)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	m, accounts := newPoolQuotaTestManager(t, server.URL, 2)
	m.pool.ObserveQuota(accounts[0].ID, pool.Quota{Headroom: 1, RemainingCredits: 90, UpdatedAt: time.Now()})
	if err := m.refreshPoolQuotas(context.Background(), ""); !errors.Is(err, errPoolQuotaFailed) {
		t.Fatalf("error=%v", err)
	}
	for _, account := range accounts {
		quota := poolQuotaByID(t, m, account.ID)
		credential, _ := m.pool.Credential(account.AuthID)
		if strings.HasSuffix(credential.APIKey, "-0") {
			if quota.Error == "" || strings.Contains(quota.Error, "synthetic") || quota.RemainingCredits != 0 || quota.Headroom != 0 || quota.UpdatedAt.IsZero() {
				t.Fatalf("failed account remained eligible: %+v", quota)
			}
		} else if quota.Error != "" || quota.RemainingCredits != 17 || quota.Headroom == 0 {
			t.Fatalf("failure reset another account: %+v", quota)
		}
	}
}

func TestPoolQuotaSecretStatusPreservesExistingKeyUpdateAndImport(t *testing.T) {
	m := newPoolHandlerManager(t)
	key := "existing-display-status-synthetic-key"
	a := addPoolHandlerAccount(t, m, key, "original", "owner")
	q := pool.Quota{Headroom: 0.2, RemainingCredits: 17, UpdatedAt: time.Now(), SubscriptionStatus: "echo " + key}
	if err := m.pool.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	for _, request := range []struct {
		endpoint string
		input    pool.AccountInput
	}{
		{"/accounts", pool.AccountInput{ID: a.ID, APIKey: key, Name: "updated", GroupID: a.GroupID, MaxConcurrency: 3}},
		{"/accounts/import", pool.AccountInput{APIKey: key, Name: "imported", GroupID: a.GroupID, MaxConcurrency: 4}},
		{"/accounts/import", pool.AccountInput{APIKey: key, Name: "imported", GroupID: a.GroupID, MaxConcurrency: 4}},
	} {
		var input any = request.input
		if request.endpoint == "/accounts/import" {
			input = map[string]any{"accounts": []pool.AccountInput{request.input}}
		}
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		resp := callPoolManagement(t, m, http.MethodPost, request.endpoint, string(body))
		if resp.StatusCode != http.StatusOK || strings.Contains(string(resp.Body), key) || !strings.Contains(string(resp.Body), "echo [redacted]") {
			t.Fatalf("existing-key %s failed or leaked status: %d %s", request.endpoint, resp.StatusCode, resp.Body)
		}
		accounts := m.pool.Snapshot()
		if len(accounts) != 1 || accounts[0].ID != a.ID || accounts[0].Name != request.input.Name || accounts[0].MaxConcurrency != request.input.MaxConcurrency || accounts[0].Quota.SubscriptionStatus != "echo [redacted]" {
			t.Fatalf("same-key operation was not an idempotent update: %+v", accounts)
		}
	}
	futureKey := "new-display-status-synthetic-key"
	q.SubscriptionStatus = "echo " + futureKey
	if err := m.pool.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(pool.AccountInput{APIKey: futureKey, Name: "new", GroupID: "new-owner", MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	resp := callPoolManagement(t, m, http.MethodPost, "/accounts", string(body))
	if resp.StatusCode != http.StatusBadRequest || len(m.pool.Snapshot()) != 1 || strings.Contains(string(resp.Body), futureKey) {
		t.Fatalf("truly new key bypassed display metadata conflict protection: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestRefreshPoolQuotasRejectsRedirectAndOversizeBody(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		_, _ = io.WriteString(w, poolQuotaTestCredits)
	}))
	defer target.Close()
	for _, mode := range []string{"redirect", "sized body", "chunked body", "invalid secret body"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
				case "sized body":
					w.Header().Set("Content-Length", fmt.Sprint(poolQuotaMaxBody+1))
					_, _ = io.WriteString(w, strings.Repeat("x", int(poolQuotaMaxBody+1)))
				case "chunked body":
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, strings.Repeat("x", int(poolQuotaMaxBody+1)))
				case "invalid secret body":
					_, _ = io.WriteString(w, `{"credits":{"monthlyCredits":"synthetic-quota-key-0"}}`)
				}
			}))
			defer server.Close()
			m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
			err := m.refreshPoolQuotas(context.Background(), accounts[0].ID)
			quota := poolQuotaByID(t, m, accounts[0].ID)
			if !errors.Is(err, errPoolQuotaFailed) || quota.Error == "" || quota.Headroom != 0 || strings.Contains(quota.Error, "synthetic") {
				t.Fatalf("error=%v snapshot=%+v", err, quota)
			}
		})
	}
	if forwarded.Load() != 0 {
		t.Fatal("redirect forwarded credential to another endpoint")
	}
}

func TestRefreshPoolQuotasConcurrencyBoundAndNoOverlap(t *testing.T) {
	var inflight, peak, starts atomic.Int32
	fourStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inflight.Add(1)
		defer inflight.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		if r.URL.Path == accountCreditsPath {
			if starts.Add(1) == poolQuotaWorkers {
				once.Do(func() { close(fourStarted) })
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, poolQuotaTestCredits)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	m, accounts := newPoolQuotaTestManager(t, server.URL, 10)
	m.cfg.RequestTimeout = 3 * time.Second
	done := make(chan error, 1)
	go func() { done <- m.refreshPoolQuotas(context.Background(), "") }()
	select {
	case <-fourStarted:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("did not fill four available worker slots")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := m.refreshPoolQuotas(ctx, accounts[0].ID); !errors.Is(err, errPoolQuotaCanceled) {
		close(release)
		t.Fatalf("manual refresh waiting on probe mutex must cancel: %v", err)
	}
	if starts.Load() != poolQuotaWorkers {
		close(release)
		t.Fatal("manual/background refresh overlapped")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() > poolQuotaWorkers || starts.Load() != 10 {
		t.Fatalf("peak HTTP inflight=%d accounts probed=%d", peak.Load(), starts.Load())
	}
}

func TestPoolQuotaLoopImmediateProbeAndCancelJoin(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	m, _ := newPoolQuotaTestManager(t, server.URL, 1)
	m.cfg.RequestTimeout = time.Minute // Effective budget is still capped at 30s.
	stop := m.startPoolQuotaLoop(0)
	select {
	case <-started:
	case <-time.After(time.Second):
		stop()
		t.Fatal("default loop did not probe immediately")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("loop stop did not cancel and join request owner promptly")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request was not canceled")
	}
	stop() // Stop is idempotent and safe to call concurrently/repeatedly.
	m.mu.Lock()
	m.pool, m.cfg = nil, config.Config{}
	m.mu.Unlock() // Joined loop must not access newly cleared lifecycle state.
}

func TestPoolQuotaLoopStopCancelsMutexWait(t *testing.T) {
	m := &Manager{}
	m.probeMu.Lock()
	stop := m.startPoolQuotaLoop(time.Hour)
	defer m.probeMu.Unlock()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop blocked on uninterruptible probe lock")
	}
}

func TestRefreshPoolQuotasDisabledAndAbsentPool(t *testing.T) {
	if err := (&Manager{}).refreshPoolQuotas(context.Background(), ""); !errors.Is(err, errPoolQuotaUnavailable) {
		t.Fatalf("absent pool error=%v", err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == accountCreditsPath {
			_, _ = io.WriteString(w, poolQuotaTestCredits)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
	disabled := false
	if _, err := m.pool.Upsert(pool.AccountInput{ID: accounts[0].ID, Name: accounts[0].Name, GroupID: accounts[0].GroupID, MaxConcurrency: 1, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := m.refreshPoolQuotas(context.Background(), ""); err != nil || calls.Load() != 0 {
		t.Fatalf("background probed disabled credential: calls=%d error=%v", calls.Load(), err)
	}
	if err := m.refreshPoolQuotas(context.Background(), accounts[0].ID); err != nil || calls.Load() != 3 {
		t.Fatalf("manual disabled credential probe failed: calls=%d error=%v", calls.Load(), err)
	}
}

func TestRefreshPoolQuotasEmptyUnknownAndTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	m, _ := newPoolQuotaTestManager(t, server.URL, 0)
	if err := m.refreshPoolQuotas(context.Background(), ""); err != nil || calls.Load() != 0 {
		t.Fatalf("empty pool caused outbound request: calls=%d error=%v", calls.Load(), err)
	}
	if err := m.refreshPoolQuotas(context.Background(), "unknown-synthetic-key"); !errors.Is(err, errPoolQuotaUnknown) || calls.Load() != 0 || strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("unknown account: calls=%d error=%v", calls.Load(), err)
	}
	m, accounts := newPoolQuotaTestManager(t, server.URL, 1)
	m.cfg.RequestTimeout = 30 * time.Millisecond
	start := time.Now()
	if err := m.refreshPoolQuotas(context.Background(), accounts[0].ID); !errors.Is(err, errPoolQuotaFailed) {
		t.Fatalf("error=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("configured short timeout was not honored")
	}
	if quota := poolQuotaByID(t, m, accounts[0].ID); quota.Headroom != 0 || quota.Error == "" || strings.Contains(quota.Error, "synthetic") || quota.UpdatedAt.IsZero() {
		t.Fatalf("timeout did not fail closed: %+v", quota)
	}
}
