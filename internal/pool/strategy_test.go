package pool

import (
	"fmt"
	"testing"
	"time"
)

// observeMonthly records a measured quota with the subscription display fields
// that expiry-first reads.
func observeMonthly(t *testing.T, p *Pool, a AccountView, headroom, credits, monthly float64, periodEnd time.Time) {
	t.Helper()
	m, end := monthly, periodEnd
	if err := p.ObserveQuota(a.ID, Quota{
		Headroom: headroom, RemainingCredits: credits, UpdatedAt: time.Now(),
		MonthlyCredits: &m, SubscriptionPeriodEnd: &end,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPickRoundRobinRotatesEveryEligibleAccount(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStrategy(StrategyRoundRobin); err != nil {
		t.Fatal(err)
	}
	low := addAccount(t, p, "rr-low-key", "g-low", 4)
	high := addAccount(t, p, "rr-high-key", "g-high", 4)
	observe(t, p, low, 0.1, 10)
	observe(t, p, high, 0.9, 10)
	ids := []string{low.AuthID, high.AuthID}
	seen := map[string]int{}
	for i := range 4 {
		decision := p.Pick(ids, fmt.Sprintf("rr-%d", i), "model")
		if decision.AuthID == "" {
			t.Fatal("round-robin returned no pick")
		}
		seen[decision.AuthID]++
	}
	if seen[low.AuthID] != 2 || seen[high.AuthID] != 2 {
		t.Fatalf("round-robin ignored headroom must still balance: %+v", seen)
	}
}

func TestPickRoundRobinFairAcrossChangingCandidateSets(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStrategy(StrategyRoundRobin); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "rr-dyn-a-key", "g-dyn-a", 4)
	b := addAccount(t, p, "rr-dyn-b-key", "g-dyn-b", 4)
	observe(t, p, a, 0.5, 10)
	observe(t, p, b, 0.5, 10)
	first, second := a, b
	if b.AuthID < a.AuthID {
		first, second = b, a
	}
	// The eligible set varies between requests (a singleton while the other
	// account is saturated). A rotation indexed by the set size starves the
	// account that is always present; the cursor must advance by identity.
	both, single := []string{a.AuthID, b.AuthID}, []string{first.AuthID}
	for i, tc := range []struct {
		ids  []string
		want string
	}{
		{both, first.AuthID},
		{single, first.AuthID},
		{both, second.AuthID},
		{both, first.AuthID},
		{single, first.AuthID},
		{both, second.AuthID},
	} {
		if got := p.Pick(tc.ids, fmt.Sprintf("rr-dyn-%d", i), "model").AuthID; got != tc.want {
			t.Fatalf("pick %d (%v) = %s, want %s", i, tc.ids, got, tc.want)
		}
	}
}

func TestPickFillFirstExhaustsBeforeSwitch(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStrategy(StrategyFillFirst); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "ff-a-key", "g-a", 4)
	b := addAccount(t, p, "ff-b-key", "g-b", 4)
	// Give the second account far more headroom and credits: fill-first must
	// still stay on the first account in auth-ID order.
	observe(t, p, a, 0.05, 1)
	observe(t, p, b, 0.95, 100)
	first, second := a, b
	if b.AuthID < a.AuthID {
		first, second = b, a
	}
	ids := []string{a.AuthID, b.AuthID}
	for i := range 3 {
		if got := p.Pick(ids, fmt.Sprintf("ff-%d", i), "model").AuthID; got != first.AuthID {
			t.Fatalf("fill-first pick %d = %s, want %s", i, got, first.AuthID)
		}
	}
	// Exhaust the first account (window/credits gone → ineligible); the next in
	// order takes over.
	observe(t, p, first, 0, 50)
	if got := p.Pick(ids, "ff-exhausted", "model").AuthID; got != second.AuthID {
		t.Fatalf("fill-first after exhaustion = %s, want %s", got, second.AuthID)
	}
}

func TestPickExpiryFirstPrefersEarliestMonthlyPeriod(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStrategy(StrategyExpiryFirst); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	soon := addAccount(t, p, "ef-soon-key", "g-soon", 4)
	later := addAccount(t, p, "ef-later-key", "g-later", 4)
	// The soon account has less headroom: expiry, not headroom, must decide.
	observeMonthly(t, p, soon, 0.3, 5, 4, now.Add(24*time.Hour))
	observeMonthly(t, p, later, 0.9, 50, 4, now.Add(72*time.Hour))
	decision := p.Pick([]string{soon.AuthID, later.AuthID}, "ef-soonest", "model")
	if decision.AuthID != soon.AuthID {
		t.Fatalf("expiry-first picked %s, want the soonest period end %s", decision.AuthID, soon.AuthID)
	}
	if decision.Strategy != StrategyExpiryFirst {
		t.Fatalf("decision strategy = %q", decision.Strategy)
	}

	// Regression: a soon period end with an empty monthly allowance (credits
	// come only from the purchased balance) has nothing to save by resetting,
	// so it must not outrank an account that still holds monthly credits.
	purchasedOnly := addAccount(t, p, "ef-purchased-key", "g-purchased", 4)
	observeMonthly(t, p, purchasedOnly, 0.95, 50, 0, now.Add(2*time.Hour))
	if got := p.Pick([]string{purchasedOnly.AuthID, later.AuthID}, "ef-empty-monthly", "model").AuthID; got != later.AuthID {
		t.Fatalf("expiry-first preferred an empty monthly allowance: %s", got)
	}

	// An already-elapsed period end is not a future expiry: backstop chain.
	elapsed := addAccount(t, p, "ef-elapsed-key", "g-elapsed", 4)
	observeMonthly(t, p, elapsed, 0.95, 50, 4, now.Add(-time.Hour))
	if got := p.Pick([]string{elapsed.AuthID, later.AuthID}, "ef-elapsed", "model").AuthID; got != later.AuthID {
		t.Fatalf("expiry-first honoured an elapsed period end: %s", got)
	}

	// Neither account carries expiry data: the backstop chain picks by headroom.
	unknownLow := addAccount(t, p, "ef-unknown-low", "g-unknown-low", 4)
	unknownHigh := addAccount(t, p, "ef-unknown-high", "g-unknown-high", 4)
	observe(t, p, unknownLow, 0.2, 50)
	observe(t, p, unknownHigh, 0.8, 50)
	if got := p.Pick([]string{unknownLow.AuthID, unknownHigh.AuthID}, "ef-backstop", "model").AuthID; got != unknownHigh.AuthID {
		t.Fatalf("backstop did not use headroom: %s", got)
	}
}

func TestSetStrategyHotSwapRejectsUnknown(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "hs-a-key", "g-a", 4)
	b := addAccount(t, p, "hs-b-key", "g-b", 4)
	observe(t, p, a, 0.2, 10)
	observe(t, p, b, 0.9, 10)
	ids := []string{a.AuthID, b.AuthID}
	if got := p.Pick(ids, "hs-default", "model").AuthID; got != b.AuthID {
		t.Fatalf("default strategy = %s, want the highest headroom", got)
	}
	if p.Strategy() != StrategyHeadroomFirst {
		t.Fatalf("default strategy = %q", p.Strategy())
	}
	if err := p.SetStrategy("nonsense"); err == nil {
		t.Fatal("SetStrategy accepted an unknown strategy")
	}
	if p.Strategy() != StrategyHeadroomFirst {
		t.Fatalf("rejected swap changed the strategy: %q", p.Strategy())
	}
	if err := p.SetStrategy(StrategyFillFirst); err != nil {
		t.Fatal(err)
	}
	first := a.AuthID
	if b.AuthID < a.AuthID {
		first = b.AuthID
	}
	decision := p.Pick(ids, "hs-fill", "model")
	if decision.AuthID != first {
		t.Fatalf("after hot swap = %s, want %s", decision.AuthID, first)
	}
	if decision.Strategy != StrategyFillFirst {
		t.Fatalf("decision strategy after swap = %q", decision.Strategy)
	}
	events := p.Events(0)
	last := events[len(events)-1]
	if last.Action != "pick" || last.Strategy != StrategyFillFirst {
		t.Fatalf("pick event did not record the strategy: %+v", last)
	}
}
