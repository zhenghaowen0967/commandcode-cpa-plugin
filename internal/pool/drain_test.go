package pool

import (
	"errors"
	"testing"
	"time"
)

func TestDisableAdmissionDoesNotCancelActiveOwner(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "drain-fixture-key", "owner", 10)
	lease, err := p.Acquire(a.AuthID, "active-before-drain", "model")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Settle("test_cleanup")
	disabled := false
	view, err := p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: a.GroupID, MaxConcurrency: 10, Enabled: &disabled})
	if err != nil || view.Enabled || view.Inflight != 1 || view.MaxConcurrency != 10 {
		t.Fatalf("admission pause changed owner or cap: view=%+v err=%v", view, err)
	}
	select {
	case <-lease.Context.Done():
		t.Fatal("pausing admission canceled the existing owner")
	default:
	}
	if _, err := p.Acquire(a.AuthID, "late-after-drain", "model"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("paused account admitted new owner: %v", err)
	}
	lease.Settle("upstream_complete")
	if p.Snapshot()[0].Inflight != 0 {
		t.Fatal("normal owner settlement did not finish drain")
	}
	enabled := true
	view, err = p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: a.GroupID, MaxConcurrency: 10, Enabled: &enabled})
	if err != nil || !view.Enabled || view.GroupID != a.GroupID || view.MaxConcurrency != 10 {
		t.Fatalf("original account settings not restored: view=%+v err=%v", view, err)
	}
	fresh, err := p.Acquire(a.AuthID, "after-resume", "model")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Settle("upstream_complete")
}
