package pool

import (
	"fmt"
	"testing"
	"time"
)

// 容量边界：表满时，既有键的改绑必须始终允许（否则旧绑定悬挂、会话粘到失去资格
// 的账号上）；只有新键才可能被拒绝。
func TestStickyRebindAllowedWhenTableFull(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	if err := p.SetStickyTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "sticky-full-a", "g-a", 4)
	b := addAccount(t, p, "sticky-full-b", "g-b", 4)
	observe(t, p, a, 0.9, 10)
	observe(t, p, b, 0.2, 10)
	ids := []string{a.AuthID, b.AuthID}

	// 绑定 s-full → a（策略：余量最高）。
	if got := p.PickWithSession(ids, "full-1", "model", "", "s-full"); got.AuthID != a.AuthID {
		t.Fatalf("bind pick = %s, want %s", got.AuthID, a.AuthID)
	}
	// 用其他会话把表填满到上限。
	for i := 0; len(p.sticky) < MaxStickySessions; i++ {
		p.bindStickyLocked(fmt.Sprintf("filler-%d", i), b.AuthID, time.Now().Add(time.Hour))
	}
	if len(p.sticky) != MaxStickySessions {
		t.Fatalf("table size = %d, want %d", len(p.sticky), MaxStickySessions)
	}
	// a 失去资格 → 回退到 b，并必须把 s-full 改绑到 b（满表也不例外）。
	observe(t, p, a, 0, 10)
	got := p.PickWithSession(ids, "full-2", "model", "", "s-full")
	if got.AuthID != b.AuthID || got.Sticky != StickyFallback {
		t.Fatalf("full-table fallback = %s sticky=%q, want %s/%q", got.AuthID, got.Sticky, b.AuthID, StickyFallback)
	}
	if binding := p.sticky["s-full"]; binding.authID != b.AuthID {
		t.Fatalf("binding not updated: %+v", binding)
	}
	// 之后 s-full 命中 b。
	observe(t, p, a, 1, 10)
	if got := p.PickWithSession(ids, "full-3", "model", "", "s-full"); got.AuthID != b.AuthID || got.Sticky != StickyHit {
		t.Fatalf("after rebind = %s sticky=%q, want %s/%q", got.AuthID, got.Sticky, b.AuthID, StickyHit)
	}
}
