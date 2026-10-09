package codexguard

import (
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRecordWindowClassification(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	future5h := now.Add(2 * time.Hour).Unix()
	futureWeek := now.Add(7 * 24 * time.Hour).Unix()

	tests := []struct {
		name    string
		headers http.Header
		reset   time.Time
		window  string
	}{
		{
			name: "primary full with noncanonical header keys",
			headers: http.Header{
				"x-codex-primary-used-percent":   {"100"},
				"x-codex-primary-reset-at":       {fmt.Sprint(future5h)},
				"x-codex-primary-window-minutes": {"300"},
			},
			reset:  time.Unix(future5h, 0),
			window: "5h",
		},
		{
			name: "secondary full",
			headers: http.Header{
				"X-Codex-Secondary-Used-Percent": {"100"},
				"X-Codex-Secondary-Reset-At":     {fmt.Sprint(futureWeek)},
			},
			reset:  time.Unix(futureWeek, 0),
			window: "week",
		},
		{
			name: "window minutes identify primary",
			headers: http.Header{
				"X-Codex-Primary-Used-Percent":   {"35"},
				"X-Codex-Primary-Reset-At":       {fmt.Sprint(future5h)},
				"X-Codex-Primary-Window-Minutes": {"300"},
			},
			reset:  time.Unix(future5h, 0),
			window: "5h",
		},
		{
			name: "window minutes identify secondary",
			headers: http.Header{
				"X-Codex-Secondary-Reset-At":       {fmt.Sprint(futureWeek)},
				"X-Codex-Secondary-Window-Minutes": {"10080"},
			},
			reset:  time.Unix(futureWeek, 0),
			window: "week",
		},
		{
			name:    "no headers falls back",
			headers: nil,
			reset:   now.Add(fallbackDuration),
			window:  "fallback-5h",
		},
		{
			name: "future reset used for a full window",
			headers: http.Header{
				"X-Codex-Primary-Used-Percent": {"100"},
				"X-Codex-Primary-Reset-At":     {fmt.Sprint(future5h)},
			},
			reset:  time.Unix(future5h, 0),
			window: "5h",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var guard Guard
			if !guard.Record(rateLimited("auth-a", tt.headers), now) {
				t.Fatal("Record() = false, want true")
			}
			got := guard.Snapshot(now)
			if len(got) != 1 {
				t.Fatalf("Snapshot() returned %d bans, want 1", len(got))
			}
			if !got[0].ResetAt.Equal(tt.reset) || got[0].Window != tt.window || got[0].AuthID != "auth-a" || !got[0].BannedAt.Equal(now) {
				t.Fatalf("ban = %#v, want reset %s, window %q", got[0], tt.reset, tt.window)
			}
		})
	}
}

func TestRecordBothFullUsesLatestValidReset(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	primaryReset := now.Add(9 * 24 * time.Hour).Unix()
	secondaryReset := now.Add(3 * 24 * time.Hour).Unix()
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":     {"100"},
		"X-Codex-Primary-Reset-At":         {fmt.Sprint(primaryReset)},
		"X-Codex-Primary-Window-Minutes":   {"300"},
		"X-Codex-Secondary-Used-Percent":   {"100"},
		"X-Codex-Secondary-Reset-At":       {fmt.Sprint(secondaryReset)},
		"X-Codex-Secondary-Window-Minutes": {"10080"},
	}

	var guard Guard
	guard.Record(rateLimited("auth-a", headers), now)
	ban := guard.Snapshot(now)[0]
	if want := time.Unix(primaryReset, 0); !ban.ResetAt.Equal(want) {
		t.Fatalf("ResetAt = %s, want latest reset %s", ban.ResetAt, want)
	}
	if ban.Window != "both" {
		t.Fatalf("Window = %q, want both", ban.Window)
	}
}

func TestRecordBothFullIgnoresInvalidResetWhenAnotherIsValid(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	future := now.Add(4 * time.Hour).Unix()
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":   {"100"},
		"X-Codex-Primary-Reset-At":       {fmt.Sprint(future)},
		"X-Codex-Secondary-Used-Percent": {"100"},
		"X-Codex-Secondary-Reset-At":     {"not-a-timestamp"},
	}
	var guard Guard
	guard.Record(rateLimited("auth-a", headers), now)
	ban := guard.Snapshot(now)[0]
	if want := time.Unix(future, 0); !ban.ResetAt.Equal(want) || ban.Window != "both" {
		t.Fatalf("ban = %#v, want valid primary reset %s and both label", ban, want)
	}
}

func TestRecordInvalidHeadersFallBackToFiveHours(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second).Unix()
	tests := []struct {
		name    string
		headers http.Header
	}{
		{name: "past reset", headers: http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(past)}}},
		{name: "invalid reset", headers: http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {"bad"}}},
		{name: "reset beyond RFC3339 range", headers: http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {"9223372036854775807"}}},
		{name: "nan percent", headers: http.Header{"X-Codex-Primary-Used-Percent": {"NaN"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(time.Hour).Unix())}}},
		{name: "infinite percent", headers: http.Header{"X-Codex-Primary-Used-Percent": {"+Inf"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(time.Hour).Unix())}}},
		{name: "invalid percent", headers: http.Header{"X-Codex-Primary-Used-Percent": {"unknown"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(time.Hour).Unix())}, "X-Codex-Primary-Window-Minutes": {"300x"}}},
		{name: "duplicate value", headers: http.Header{"X-Codex-Primary-Used-Percent": {"100", "100"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(time.Hour).Unix())}}},
		{name: "duplicate header key by case", headers: http.Header{"X-Codex-Primary-Used-Percent": {"100"}, "x-codex-primary-used-percent": {"100"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(time.Hour).Unix())}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var guard Guard
			guard.Record(rateLimited("auth-a", tt.headers), now)
			ban := guard.Snapshot(now)[0]
			if want := now.Add(fallbackDuration); !ban.ResetAt.Equal(want) || ban.Window != "fallback-5h" {
				t.Fatalf("ban = %#v, want fallback reset %s", ban, want)
			}
		})
	}
}

func TestRecordRejectsNonCodexNon429SuccessAndEmptyAuth(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		modify func(*pluginapi.UsageRecord)
	}{
		{name: "non-codex", modify: func(r *pluginapi.UsageRecord) { r.Provider = "openai" }},
		{name: "success", modify: func(r *pluginapi.UsageRecord) { r.Failed = false }},
		{name: "non-429", modify: func(r *pluginapi.UsageRecord) { r.Failure.StatusCode = 500 }},
		{name: "empty auth id", modify: func(r *pluginapi.UsageRecord) { r.AuthID = " \t " }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var guard Guard
			record := rateLimited("auth-a", nil)
			tt.modify(&record)
			if guard.Record(record, now) {
				t.Fatal("Record() = true, want false")
			}
			if got := guard.Snapshot(now); len(got) != 0 {
				t.Fatalf("Snapshot() = %#v, want no bans", got)
			}
		})
	}
}

func TestRecordRepeated429NeverShortensAndCanExtend(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	guard.Record(rateLimited("auth-a", futureFullHeader(now.Add(4*time.Hour))), now)

	guard.Record(rateLimited("auth-a", futureFullHeader(now.Add(2*time.Hour))), now.Add(time.Hour))
	ban := guard.Snapshot(now.Add(time.Hour))[0]
	if want := now.Add(4 * time.Hour); !ban.ResetAt.Equal(want) {
		t.Fatalf("valid retry shortened reset to %s, want %s", ban.ResetAt, want)
	}

	later := now.Add(6 * time.Hour)
	guard.Record(rateLimited("auth-a", futureFullHeader(later.Add(5*time.Hour))), later)
	ban = guard.Snapshot(later)[0]
	if want := later.Add(5 * time.Hour); !ban.ResetAt.Equal(want) {
		t.Fatalf("extended retry reset = %s, want %s", ban.ResetAt, want)
	}
}

func TestImportRestoresSnapshotAndFilterUsesImportedBan(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var source Guard
	source.Record(rateLimited("auth-import", futureFullHeader(now.Add(4*time.Hour))), now)
	snapshot := source.Snapshot(now)

	var restored Guard
	if got := restored.Import(snapshot, now); got != 1 {
		t.Fatalf("Import() changed %d bans, want 1", got)
	}
	if got := restored.Snapshot(now); !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("restored Snapshot() = %#v, want %#v", got, snapshot)
	}
	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "auth-import", Provider: "codex"}}
	available, blocked := restored.Filter(candidates, now)
	if !blocked || len(available) != 0 {
		t.Fatalf("Filter() = (%v, %v), want no available auth and blocked", available, blocked)
	}
}

func TestImportNeverShortensExistingBanAndCanExtendIt(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	guard.Record(rateLimited("auth-import", futureFullHeader(now.Add(4*time.Hour))), now)

	if got := guard.Import([]Ban{{AuthID: "auth-import", ResetAt: now.Add(2 * time.Hour), Window: "imported"}}, now); got != 0 {
		t.Fatalf("short import changed %d bans, want 0", got)
	}
	if got := guard.Snapshot(now)[0].ResetAt; !got.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("short import reset = %s, want existing %s", got, now.Add(4*time.Hour))
	}
	if got := guard.Import([]Ban{{AuthID: "auth-import", ResetAt: now.Add(6 * time.Hour), Window: "imported"}}, now); got != 1 {
		t.Fatalf("long import changed %d bans, want 1", got)
	}
	if got := guard.Snapshot(now)[0].ResetAt; !got.Equal(now.Add(6 * time.Hour)) {
		t.Fatalf("extended reset = %s, want %s", got, now.Add(6*time.Hour))
	}
	if got := guard.Import([]Ban{{AuthID: "auth-import", ResetAt: now.Add(3 * time.Hour), Window: "imported"}}, now); got != 0 {
		t.Fatalf("duplicate shorter import changed %d bans, want 0", got)
	}
}

func TestConcurrentRecordAndImportKeepLatestReset(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	wantReset := now.Add(8 * time.Hour)
	var guard Guard
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		guard.Record(rateLimited("auth-race", futureFullHeader(now.Add(2*time.Hour))), now)
	}()
	go func() {
		defer wg.Done()
		<-start
		guard.Import([]Ban{{AuthID: "auth-race", ResetAt: wantReset, Window: "imported"}}, now)
	}()
	close(start)
	wg.Wait()
	got := guard.Snapshot(now)
	if len(got) != 1 || !got[0].ResetAt.Equal(wantReset) {
		t.Fatalf("concurrent Record/Import snapshot = %#v, want reset %s", got, wantReset)
	}
}

func TestImportIgnoresExpiredBan(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	if got := guard.Import([]Ban{{AuthID: "expired", ResetAt: now}}, now); got != 0 {
		t.Fatalf("expired import changed %d bans, want 0", got)
	}
	if got := guard.Snapshot(now); len(got) != 0 {
		t.Fatalf("Snapshot() = %#v, want empty", got)
	}
}

func TestFilterExpiresBanAndSnapshotSortsAndCleans(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	guard.Record(rateLimited("z-auth", nil), now)
	guard.Record(rateLimited("a-auth", nil), now)
	guard.Record(rateLimited("m-auth", nil), now)

	bans := guard.Snapshot(now)
	ids := []string{bans[0].AuthID, bans[1].AuthID, bans[2].AuthID}
	if want := []string{"a-auth", "m-auth", "z-auth"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("sorted AuthIDs = %v, want %v", ids, want)
	}

	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "a-auth", Provider: "codex"},
		{ID: "m-auth", Provider: "CoDeX"},
		{ID: "outside", Provider: "openai"},
	}
	available, blocked := guard.Filter(candidates, now.Add(fallbackDuration-time.Second))
	if !blocked || !reflect.DeepEqual(available, []pluginapi.SchedulerAuthCandidate{{ID: "outside", Provider: "openai"}}) {
		t.Fatalf("Filter before expiry = (%v, %v), want only non-Codex and blocked", available, blocked)
	}

	available, blocked = guard.Filter(candidates, now.Add(fallbackDuration))
	if blocked || !reflect.DeepEqual(available, candidates) {
		t.Fatalf("Filter at expiry = (%v, %v), want all candidates and not blocked", available, blocked)
	}
	if got := guard.Snapshot(now.Add(fallbackDuration)); len(got) != 0 {
		t.Fatalf("Snapshot after expiry = %#v, want empty", got)
	}
}

func TestUnbanAndClearAll(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	guard.Record(rateLimited("auth-a", nil), now)
	guard.Record(rateLimited("auth-b", nil), now)

	if !guard.Unban("auth-a") || guard.Unban("auth-a") || guard.Unban(" ") {
		t.Fatal("Unban() did not report removal exactly once")
	}
	if got := guard.ClearAll(); got != 1 {
		t.Fatalf("ClearAll() = %d, want 1", got)
	}
	if got := guard.ClearAll(); got != 0 {
		t.Fatalf("second ClearAll() = %d, want 0", got)
	}
	if got := guard.Snapshot(now); got == nil || len(got) != 0 {
		t.Fatalf("Snapshot() = %#v, want non-nil empty slice", got)
	}
}

func TestFilterDoesNotBlockNonCodexOrUnbannedCodex(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var guard Guard
	guard.Record(rateLimited("blocked", nil), now)
	candidates := []pluginapi.SchedulerAuthCandidate{
		{ID: "blocked", Provider: "CODEx"},
		{ID: "free", Provider: "codex"},
		{ID: "other", Provider: "openai"},
	}
	available, blocked := guard.Filter(candidates, now)
	want := candidates[1:]
	if !blocked || !reflect.DeepEqual(available, want) {
		t.Fatalf("Filter() = (%v, %v), want (%v, true)", available, blocked, want)
	}
	available, blocked = guard.Filter(candidates[1:], now)
	if blocked || !reflect.DeepEqual(available, candidates[1:]) {
		t.Fatalf("Filter() without banned candidates = (%v, %v), want unchanged and false", available, blocked)
	}
}

func TestGuardConcurrentAccess(t *testing.T) {
	var guard Guard
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	candidates := []pluginapi.SchedulerAuthCandidate{{ID: "auth-1", Provider: "codex"}, {ID: "auth-2", Provider: "openai"}}
	const workers = 12
	const iterations = 250
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				id := fmt.Sprintf("auth-%d", (worker+i)%4)
				guard.Record(rateLimited(id, nil), now)
				guard.Filter(candidates, now)
				guard.Snapshot(now)
				if i%17 == 0 {
					guard.Unban(id)
				}
				if i%61 == 0 {
					guard.ClearAll()
				}
			}
		}(worker)
	}
	wg.Wait()
}

func rateLimited(authID string, headers http.Header) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Provider:        "Codex",
		AuthType:        "oauth",
		AuthID:          authID,
		Failed:          true,
		Failure:         pluginapi.UsageFailure{StatusCode: statusTooMany},
		ResponseHeaders: headers,
	}
}

func futureFullHeader(resetAt time.Time) http.Header {
	return http.Header{
		"X-Codex-Primary-Used-Percent": {fmt.Sprint(100.0)},
		"X-Codex-Primary-Reset-At":     {fmt.Sprint(resetAt.Unix())},
	}
}
