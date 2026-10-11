package pool

import (
	"fmt"
	"testing"
	"time"
)

// 默认不启用：会话键被忽略，仍按策略选号。
func TestStickyDisabledByDefault(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "sticky-off-a", "g-a", 4)
	b := addAccount(t, p, "sticky-off-b", "g-b", 4)
	observe(t, p, a, 0.2, 10)
	observe(t, p, b, 0.9, 10)
	ids := []string{a.AuthID, b.AuthID}
	first := p.PickWithSession(ids, "off-1", "model", "", "session-1")
	if first.AuthID != b.AuthID || first.Sticky != StickyOff {
		t.Fatalf("default pick = %s sticky=%q, want %s/%q", first.AuthID, first.Sticky, b.AuthID, StickyOff)
	}
	// 与第一次不同的会话同样走策略（余量高的 B），说明没有粘性状态。
	second := p.PickWithSession(ids, "off-2", "model", "", "session-2")
	if second.AuthID != b.AuthID || second.Sticky != StickyOff {
		t.Fatalf("second pick = %s sticky=%q", second.AuthID, second.Sticky)
	}
}

// 会话绑定后即使另一个账号余量更高也保持同一账号；新会话按策略。
func TestStickySessionKeepsBoundAccount(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "sticky-hit-a", "g-a", 4)
	b := addAccount(t, p, "sticky-hit-b", "g-b", 4)
	observe(t, p, a, 0.2, 10)
	observe(t, p, b, 0.9, 10)
	ids := []string{a.AuthID, b.AuthID}

	bind := p.PickWithSession(ids, "hit-1", "model", "", "session-x")
	if bind.AuthID != b.AuthID || bind.Sticky != StickyMiss {
		t.Fatalf("first pick = %s sticky=%q, want %s/%q", bind.AuthID, bind.Sticky, b.AuthID, StickyMiss)
	}
	if events := p.Events(0); events[len(events)-1].Sticky != StickyMiss {
		t.Fatalf("pick event sticky = %q", events[len(events)-1].Sticky)
	}

	// A 现在余量更高，但 session-x 已绑定 B：仍选 B。
	observe(t, p, a, 1, 10)
	observe(t, p, b, 0.3, 10)
	hit := p.PickWithSession(ids, "hit-2", "model", "", "session-x")
	if hit.AuthID != b.AuthID || hit.Sticky != StickyHit {
		t.Fatalf("sticky pick = %s sticky=%q, want %s/%q", hit.AuthID, hit.Sticky, b.AuthID, StickyHit)
	}

	// 另一个会话按策略走（此时 A 余量最高）。
	other := p.PickWithSession(ids, "hit-3", "model", "", "session-y")
	if other.AuthID != a.AuthID || other.Sticky != StickyMiss {
		t.Fatalf("other session = %s sticky=%q, want %s/%q", other.AuthID, other.Sticky, a.AuthID, StickyMiss)
	}
	// 无会话键的请求不受粘性影响。
	none := p.PickWithSession(ids, "hit-4", "model", "", "  ")
	if none.AuthID != a.AuthID || none.Sticky != StickyOff {
		t.Fatalf("keyless pick = %s sticky=%q", none.AuthID, none.Sticky)
	}
}

// 绑定账号失去资格时回退策略并改绑；恢复资格后仍跟随新绑定。
func TestStickySessionFallsBackWhenBoundAccountIneligible(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "sticky-fb-a", "g-a", 4)
	b := addAccount(t, p, "sticky-fb-b", "g-b", 4)
	observe(t, p, a, 0.9, 10)
	observe(t, p, b, 0.2, 10)
	ids := []string{a.AuthID, b.AuthID}

	if got := p.PickWithSession(ids, "fb-1", "model", "", "s-fb"); got.AuthID != a.AuthID {
		t.Fatalf("bind pick = %s, want %s", got.AuthID, a.AuthID)
	}
	observe(t, p, a, 0, 10) // 额度耗尽 → 失去资格
	fallback := p.PickWithSession(ids, "fb-2", "model", "", "s-fb")
	if fallback.AuthID != b.AuthID || fallback.Sticky != StickyFallback {
		t.Fatalf("fallback pick = %s sticky=%q, want %s/%q", fallback.AuthID, fallback.Sticky, b.AuthID, StickyFallback)
	}
	observe(t, p, a, 1, 10) // A 恢复资格，但绑定已改到 B
	if got := p.PickWithSession(ids, "fb-3", "model", "", "s-fb"); got.AuthID != b.AuthID || got.Sticky != StickyHit {
		t.Fatalf("after rebind = %s sticky=%q, want %s/%q", got.AuthID, got.Sticky, b.AuthID, StickyHit)
	}
}

// 窗口过期后重新按策略选择并重新绑定。
func TestStickySessionExpires(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStickyTTL(20 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "sticky-ttl-a", "g-a", 4)
	b := addAccount(t, p, "sticky-ttl-b", "g-b", 4)
	observe(t, p, a, 0.2, 10)
	observe(t, p, b, 0.9, 10)
	ids := []string{a.AuthID, b.AuthID}

	if got := p.PickWithSession(ids, "ttl-1", "model", "", "s-ttl"); got.AuthID != b.AuthID {
		t.Fatalf("bind pick = %s", got.AuthID)
	}
	observe(t, p, a, 1, 10) // 策略现在会选 A
	hit := p.PickWithSession(ids, "ttl-2", "model", "", "s-ttl")
	if hit.AuthID != b.AuthID || hit.Sticky != StickyHit {
		t.Fatalf("before expiry = %s sticky=%q", hit.AuthID, hit.Sticky)
	}
	time.Sleep(80 * time.Millisecond)
	expired := p.PickWithSession(ids, "ttl-3", "model", "", "s-ttl")
	if expired.AuthID != a.AuthID || expired.Sticky != StickyFallback {
		t.Fatalf("after expiry = %s sticky=%q, want %s/%q", expired.AuthID, expired.Sticky, a.AuthID, StickyFallback)
	}
}

// 关闭粘性会清空已有绑定；TTL 越界被拒绝且不改变当前值。
func TestSetStickyTTLValidation(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "sticky-val-a", "g-a", 4)
	b := addAccount(t, p, "sticky-val-b", "g-b", 4)
	observe(t, p, a, 0.9, 10)
	observe(t, p, b, 0.2, 10)
	ids := []string{a.AuthID, b.AuthID}
	if err := p.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := p.PickWithSession(ids, "val-1", "model", "", "s-val"); got.AuthID != a.AuthID {
		t.Fatalf("bind pick = %s", got.AuthID)
	}
	for _, bad := range []time.Duration{-time.Second, MaxStickyTTL + time.Second} {
		if err := p.SetStickyTTL(bad); err == nil {
			t.Fatalf("accepted sticky ttl %s", bad)
		}
	}
	if p.StickyTTL() != time.Hour {
		t.Fatalf("rejected ttl changed the window: %s", p.StickyTTL())
	}
	if err := p.SetStickyTTL(0); err != nil {
		t.Fatal(err)
	}
	if p.StickyTTL() != 0 {
		t.Fatalf("disable failed: %s", p.StickyTTL())
	}
	if got := p.PickWithSession(ids, "val-2", "model", "", "s-val"); got.AuthID != a.AuthID || got.Sticky != StickyOff {
		t.Fatalf("after disable = %s sticky=%q", got.AuthID, got.Sticky)
	}
}

// 粘性命中不推进策略轮询游标：轮询策略下命中的账号保持一致。
func TestStickyHitDoesNotAdvanceRoundRobin(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStrategy(StrategyRoundRobin); err != nil {
		t.Fatal(err)
	}
	if err := p.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "sticky-rr-a", "g-a", 4)
	b := addAccount(t, p, "sticky-rr-b", "g-b", 4)
	observe(t, p, a, 0.5, 10)
	observe(t, p, b, 0.5, 10)
	ids := []string{a.AuthID, b.AuthID}
	bound := p.PickWithSession(ids, "rr-1", "model", "", "s-rr")
	for i := range 3 {
		got := p.PickWithSession(ids, fmt.Sprintf("rr-hit-%d", i), "model", "", "s-rr")
		if got.AuthID != bound.AuthID || got.Sticky != StickyHit {
			t.Fatalf("sticky round-robin pick = %s sticky=%q, want %s/%q", got.AuthID, got.Sticky, bound.AuthID, StickyHit)
		}
	}
	// 无会话的请求仍按轮询前进（游标未被粘性命中改动）。
	seen := map[string]int{}
	for i := range 2 {
		got := p.PickWithSession(ids, fmt.Sprintf("rr-free-%d", i), "model", "", "")
		seen[got.AuthID]++
	}
	if seen[a.AuthID] != 1 || seen[b.AuthID] != 1 {
		t.Fatalf("round-robin after sticky hits = %+v", seen)
	}
}
