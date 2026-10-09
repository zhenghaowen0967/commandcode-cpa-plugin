package pool

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func testPool(t *testing.T, ttl time.Duration) (*Pool, Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{AuthDir: filepath.Join(dir, "auth"), StatePath: filepath.Join(dir, "private", "state.json"), QuotaMaxAge: ttl, DefaultLimit: 1}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, cfg
}
func addAccount(t *testing.T, p *Pool, key, group string, cap int) AccountView {
	t.Helper()
	a, err := p.Upsert(AccountInput{Name: "Test account", GroupID: group, APIKey: key, MaxConcurrency: cap})
	if err != nil {
		t.Fatal(err)
	}
	observe(t, p, a, 1, 10)
	return a
}
func observe(t *testing.T, p *Pool, a AccountView, headroom, credits float64) {
	t.Helper()
	if err := p.ObserveQuota(a.ID, Quota{Headroom: headroom, RemainingCredits: credits, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}
func findView(t *testing.T, p *Pool, id string) AccountView {
	t.Helper()
	for _, a := range p.Snapshot() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("missing account %s", id)
	return AccountView{}
}

func TestCredentialIdentityPersistenceAndTombstone(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	key := "synthetic-test-key-one"
	a := addAccount(t, p, key, "real-account-a", 2)
	if a.ID != keyHash(key) || a.AuthID != ProviderID+"-key-"+keyHash(key)+".json" {
		t.Fatalf("identity mismatch %#v", a)
	}
	for _, path := range []string{cfg.StatePath, filepath.Join(cfg.AuthDir, a.AuthID)} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file mode %v %v", info, err)
		}
	}
	for _, dir := range []string{cfg.AuthDir, filepath.Dir(cfg.StatePath)} {
		info, _ := os.Stat(dir)
		if info.Mode().Perm() != 0700 {
			t.Fatalf("directory mode %o", info.Mode().Perm())
		}
	}
	if second, err := New(cfg); err == nil {
		second.Close()
		t.Fatal("second instance bypassed flock")
	}
	p.Close()
	reloaded, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	c, ok := reloaded.Credential(a.AuthID)
	if !ok || c.APIKey != key || !c.Enabled {
		t.Fatal("credential lost on reload")
	}
	if view := findView(t, reloaded, a.ID); view.Inflight != 0 || view.Status != "quota_stale" {
		t.Fatalf("runtime/quota survived restart: %#v", view)
	}
	if err := reloaded.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Snapshot()) != 0 {
		t.Fatal("deleted account visible")
	}
	c, ok = reloaded.Credential(a.AuthID)
	if !ok || c.APIKey != "" || c.Enabled {
		t.Fatal("missing disabled tombstone")
	}
	if _, err := os.Stat(filepath.Join(cfg.AuthDir, a.AuthID)); !os.IsNotExist(err) {
		t.Fatal("deleted auth secret retained")
	}
	data, _ := os.ReadFile(cfg.StatePath)
	if bytes.Contains(data, []byte(key)) {
		t.Fatal("deleted state secret retained")
	}
	if _, err := reloaded.Upsert(AccountInput{Name: "revive", GroupID: "real-account-a", APIKey: key, MaxConcurrency: 2}); err == nil {
		t.Fatal("tombstone resurrected")
	}
	reloaded.Close()
	again, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	c, ok = again.Credential(a.AuthID)
	if !ok || c.Enabled || c.APIKey != "" {
		t.Fatal("tombstone lost on reload")
	}
}

func TestUpsertDuplicatesAndGroupCapContract(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "synthetic-key-a", "owner-a", 2)
	b := addAccount(t, p, "synthetic-key-b", "owner-a", 2)
	if _, err := p.Upsert(AccountInput{Name: "new", GroupID: "owner-a", APIKey: "synthetic-key-c", MaxConcurrency: 3}); err == nil {
		t.Fatal("new key changed group cap")
	}
	disabled := false
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "off", GroupID: a.GroupID, MaxConcurrency: 2, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	dup, err := p.Upsert(AccountInput{Name: "duplicate", GroupID: a.GroupID, APIKey: "synthetic-key-a", MaxConcurrency: 2})
	if err != nil || dup.ID != a.ID || dup.Enabled {
		t.Fatal("duplicate revived disabled key", err)
	}
	if _, err := p.Upsert(AccountInput{Name: "duplicate", GroupID: "wrong-owner", APIKey: "synthetic-key-a", MaxConcurrency: 2}); err == nil {
		t.Fatal("same key moved group without explicit ID")
	}
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "rotate", GroupID: a.GroupID, APIKey: "other-key", MaxConcurrency: 2}); err == nil {
		t.Fatal("in-place key rotation permitted")
	}
	if _, err := p.Upsert(AccountInput{ID: b.ID, Name: b.Name, GroupID: b.GroupID, MaxConcurrency: 3}); err != nil {
		t.Fatal(err)
	}
	if findView(t, p, a.ID).MaxConcurrency != 3 {
		t.Fatal("group cap not propagated")
	}
	enabled := true
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: a.GroupID, MaxConcurrency: 3, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	leases := make([]*Lease, 0)
	for i := 0; i < 2; i++ {
		l, err := p.Acquire(a.AuthID, fmt.Sprintf("request-%d", i), "model")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	if _, err := p.Upsert(AccountInput{ID: b.ID, Name: b.Name, GroupID: b.GroupID, MaxConcurrency: 1}); err == nil {
		t.Fatal("lowered group below occupancy")
	}
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: "new-owner", MaxConcurrency: 3}); err == nil {
		t.Fatal("moved active account")
	}
	for _, l := range leases {
		l.Settle("done")
	}
}

func TestPickQuotaOrderingFairnessAndNoReservation(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "quota-a", "owner-a", 2)
	b := addAccount(t, p, "quota-b", "owner-b", 2)
	ids := []string{b.AuthID, a.AuthID}
	observe(t, p, a, .8, 1)
	observe(t, p, b, .7, 100)
	if d := p.Pick(ids, "req", "model"); d.AuthID != a.AuthID || len(d.Candidates) != 2 {
		t.Fatalf("headroom not primary %#v", d)
	}
	if findView(t, p, a.ID).Inflight != 0 {
		t.Fatal("pick created lease")
	}
	observe(t, p, b, .9, 100)
	if p.Pick(ids, "req", "model").AuthID != b.AuthID {
		t.Fatal("quota update ignored")
	}
	observe(t, p, a, .9, 101)
	if p.Pick(ids, "req", "model").AuthID != a.AuthID {
		t.Fatal("credit not secondary")
	}
	observe(t, p, b, .9, 101)
	lease, err := p.Acquire(a.AuthID, "active", "model")
	if err != nil {
		t.Fatal(err)
	}
	if p.Pick(ids, "req", "model").AuthID != b.AuthID {
		t.Fatal("occupancy not tertiary")
	}
	lease.Settle("done")
	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		counts[p.Pick(ids, "req", "model").AuthID]++
	}
	if counts[a.AuthID] != 5 || counts[b.AuthID] != 5 {
		t.Fatalf("unfair ties %#v", counts)
	}
	d := p.Pick([]string{a.AuthID, a.AuthID, "unknown"}, "", "model")
	if d.AuthID != "" || len(d.Candidates) != 2 {
		t.Fatal("missing request admitted or duplicate candidate")
	}
}

func TestGroupConcurrencyFanoutCancellationSettleOnce(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "fanout-a", "single-real-owner", 3)
	b := addAccount(t, p, "fanout-b", "single-real-owner", 3)
	var wg sync.WaitGroup
	var admitted atomic.Int32
	leases := make(chan *Lease, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := a.AuthID
			if i%2 != 0 {
				id = b.AuthID
			}
			l, err := p.Acquire(id, fmt.Sprintf("fanout-%d", i), "model")
			if err == nil {
				admitted.Add(1)
				leases <- l
			} else if !errors.Is(err, ErrAtCapacity) {
				t.Errorf("unexpected admission error %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(leases)
	if admitted.Load() != 3 {
		t.Fatalf("group cap bypassed %d", admitted.Load())
	}
	var owners []*Lease
	for l := range leases {
		owners = append(owners, l)
		l.Cancel()
	}
	if findView(t, p, a.ID).Inflight != 3 || findView(t, p, b.ID).Inflight != 3 {
		t.Fatal("cancel released capacity")
	}
	for _, l := range owners {
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(l *Lease) { defer wg.Done(); l.Cancel(); l.Settle("done") }(l)
		}
	}
	wg.Wait()
	if findView(t, p, a.ID).Inflight != 0 {
		t.Fatal("settle not exactly once")
	}
}

func TestAbortAndQuarantinedOwnerRetainCapacityAndLock(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "abort-key", "owner", 2)
	l, err := p.Acquire(a.AuthID, "request-a", "model")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := p.Acquire(a.AuthID, "request-a", "model")
	if err != nil {
		t.Fatal(err)
	}
	p.AbortRequest("request-a")
	for _, owner := range []*Lease{l, sibling} {
		select {
		case <-owner.Context.Done():
		default:
			t.Fatal("attempt not cancelled")
		}
	}
	if _, err := p.Acquire(a.AuthID, "request-a", "model"); !errors.Is(err, ErrAborted) {
		t.Fatal("late attempt admitted", err)
	}
	l.Record("upstream_cleanup_unconfirmed")
	if v := findView(t, p, a.ID); v.Status != "cleanup_unconfirmed" || v.Inflight != 2 {
		t.Fatalf("quarantine did not retain owner %#v", v)
	}
	if _, err := p.Acquire(a.AuthID, "request-b", "model"); !errors.Is(err, ErrAtCapacity) {
		t.Fatal("quarantine bypassed", err)
	}
	p.Close()
	if _, err := New(cfg); err == nil {
		t.Fatal("lock released with unsettled owners")
	}
	sibling.Settle("done")
	if _, err := New(cfg); err == nil {
		t.Fatal("lock released with quarantined owner")
	}
	l.Settle("confirmed_cleanup")
	next, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}

func TestQuotaFailClosedAndKnownIdentity(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "quota-finite-a", "owner-a", 1)
	b := addAccount(t, p, "quota-finite-b", "owner-b", 1)
	invalid := []Quota{
		{Headroom: math.NaN(), RemainingCredits: 10, UpdatedAt: time.Now()},
		{Headroom: 1, RemainingCredits: math.Inf(1), UpdatedAt: time.Now()},
		{Headroom: -1, RemainingCredits: 10, UpdatedAt: time.Now()},
		{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now().Add(time.Hour)},
		{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Windows: []Window{{Used: -1}}},
		{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Error: "raw synthetic-key-secret"},
	}
	for _, q := range invalid {
		if p.ObserveQuota(a.ID, q) == nil {
			t.Fatal("accepted invalid quota")
		}
		if _, err := p.Acquire(a.AuthID, "req", "model"); !errors.Is(err, ErrQuotaUnavailable) {
			t.Fatal("invalid quota remained eligible")
		}
		observe(t, p, a, 1, 10)
	}
	for _, q := range []Quota{
		{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now().Add(-2 * time.Minute)},
		{Headroom: 0, RemainingCredits: 10, UpdatedAt: time.Now()},
		{Headroom: 1, RemainingCredits: 0, UpdatedAt: time.Now()},
	} {
		_ = p.ObserveQuota(a.ID, q)
		if _, err := p.Acquire(a.AuthID, "req", "model"); !errors.Is(err, ErrQuotaUnavailable) {
			t.Fatal("stale/exhausted quota admitted")
		}
	}
	for _, acct := range []AccountView{a, b} {
		if err := p.ObserveQuota(acct.ID, Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Identity: "same-real-user"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, acct := range []AccountView{a, b} {
		if _, err := p.Acquire(acct.AuthID, "req", "model"); !errors.Is(err, ErrDisabled) {
			t.Fatal("whoami identity conflict bypassed")
		}
	}
	observe(t, p, a, 1, 10) // transient whoami unavailable; old identity must survive
	if findView(t, p, a.ID).Status != "identity_conflict" {
		t.Fatal("empty probe cleared identity")
	}
	p.Close()
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	observe(t, restored, a, 1, 10)
	observe(t, restored, b, 1, 10)
	if restored.Pick([]string{a.AuthID, b.AuthID}, "req", "model").AuthID != "" {
		t.Fatal("restart erased known identity conflict")
	}
}

func TestAtomicStorageRollbackAndSafeErrors(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "rollback-secret", "owner", 1)
	oldState, _ := os.ReadFile(cfg.StatePath)
	oldAuth, _ := os.ReadFile(filepath.Join(cfg.AuthDir, a.AuthID))
	p.rename = func(src, dst string) error {
		if dst == cfg.StatePath {
			return errors.New("injected private path and rollback-secret")
		}
		return os.Rename(src, dst)
	}
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "changed", GroupID: a.GroupID, MaxConcurrency: 2}); !errors.Is(err, ErrStorage) || strings.Contains(err.Error(), "rollback-secret") {
		t.Fatal("unsafe or missing error", err)
	}
	state, _ := os.ReadFile(cfg.StatePath)
	auth, _ := os.ReadFile(filepath.Join(cfg.AuthDir, a.AuthID))
	if !bytes.Equal(state, oldState) || !bytes.Equal(auth, oldAuth) || findView(t, p, a.ID).MaxConcurrency != 1 {
		t.Fatal("failed transaction advanced storage or admission")
	}
	if err := p.Delete(a.ID); !errors.Is(err, ErrStorage) {
		t.Fatal("expected delete rollback")
	}
	if c, ok := p.Credential(a.AuthID); !ok || !c.Enabled || c.APIKey == "" {
		t.Fatal("failed delete removed memory credential")
	}
	auth, _ = os.ReadFile(filepath.Join(cfg.AuthDir, a.AuthID))
	if !bytes.Equal(auth, oldAuth) {
		t.Fatal("failed delete lost auth secret")
	}
	p.rename = os.Rename
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "changed", GroupID: a.GroupID, MaxConcurrency: 2}); err != nil {
		t.Fatal("did not recover from ordinary failure", err)
	}
	files, _ := filepath.Glob(filepath.Join(cfg.AuthDir, ".commandcode-pool-tmp-*"))
	if len(files) > 0 {
		t.Fatal("staged secrets leaked")
	}
}

func TestEventsRingAndOutputDoNotExposeSecrets(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	key := "synthetic-event-secret"
	a := addAccount(t, p, key, "owner", 1)
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "name " + key, GroupID: a.GroupID, MaxConcurrency: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("secret metadata accepted", err)
	}
	l, err := p.Acquire(a.AuthID, "req "+key, "model "+key)
	if err != nil {
		t.Fatal(err)
	}
	l.Record("payload " + key)
	l.Settle("settle " + key)
	for _, event := range p.Events(0) {
		if event.Action == "quarantined" && event.Reason != "cleanup_unconfirmed" {
			t.Fatal("raw payload reason escaped quarantine audit")
		}
		if event.Action == "settle" && event.Reason != "owner_finished" {
			t.Fatal("raw payload reason escaped settlement audit")
		}
	}
	for i := 0; i < 600; i++ {
		p.Pick([]string{a.AuthID, "unknown " + key}, "req "+key, "model "+key)
	}
	events := p.Events(0)
	if len(events) != 500 || events[0].Sequence <= 1 {
		t.Fatal("event ring not bounded")
	}
	after := events[len(events)-2].Sequence
	if len(p.Events(after)) != 1 {
		t.Fatal("event cursor wrong")
	}
	payload, _ := json.Marshal(struct {
		Events   []Event
		Accounts []AccountView
	}{events, p.Snapshot()})
	if bytes.Contains(payload, []byte(key)) {
		t.Fatal("secret in diagnostics")
	}
	events[0].Reason = "mutated"
	events[0].Candidates[0].Reason = "mutated"
	if p.Events(0)[0].Reason == "mutated" || p.Events(0)[0].Candidates[0].Reason == "mutated" {
		t.Fatal("caller mutated event storage")
	}
}

func TestOwnedFilesOnlyAndSymlinkFailClosed(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	untouched := filepath.Join(cfg.AuthDir, "other-provider.json")
	if err := os.WriteFile(untouched, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	a := addAccount(t, p, "owned-key", "owner", 1)
	if err := p.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(untouched)
	if string(data) != "unrelated" {
		t.Fatal("unrelated auth file touched")
	}
	p.Close()
	if err := os.Remove(cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(untouched, cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); !errors.Is(err, ErrStorage) {
		t.Fatal("state symlink accepted")
	}
}

func TestExistingAuthDirectoryPermissionsPreserved(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(authDir, 0775); err != nil {
		t.Fatal(err)
	}
	beforeDir, err := os.Stat(authDir)
	if err != nil {
		t.Fatal(err)
	}
	beforeOwner := beforeDir.Sys().(*syscall.Stat_t)
	unrelated := filepath.Join(authDir, "other-provider.json")
	if err := os.WriteFile(unrelated, []byte("unrelated"), 0640); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AuthDir: authDir, StatePath: filepath.Join(root, "private", "state.json"), DefaultLimit: 1}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	a := addAccount(t, p, "shared-auth-dir-key", "owner", 1)

	info, err := os.Stat(authDir)
	if err != nil || info.Mode().Perm() != 0775 {
		t.Fatalf("existing auth directory mode changed: mode=%v err=%v", info, err)
	}
	afterOwner := info.Sys().(*syscall.Stat_t)
	if afterOwner.Uid != beforeOwner.Uid || afterOwner.Gid != beforeOwner.Gid {
		t.Fatalf("existing auth directory ownership changed: before=%d:%d after=%d:%d", beforeOwner.Uid, beforeOwner.Gid, afterOwner.Uid, afterOwner.Gid)
	}
	data, err := os.ReadFile(unrelated)
	if err != nil || string(data) != "unrelated" {
		t.Fatalf("unrelated auth file changed: data=%q err=%v", data, err)
	}
	info, err = os.Stat(unrelated)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("unrelated auth file mode changed: mode=%v err=%v", info, err)
	}
	for _, path := range []string{
		cfg.StatePath,
		filepath.Join(cfg.AuthDir, a.AuthID),
		filepath.Join(cfg.AuthDir, ".commandcode-pool.lock"),
		cfg.StatePath + ".lock",
		cfg.StatePath + ".journal.lock",
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("owned file %s mode=%v err=%v", path, info, err)
		}
	}
	if err := p.writeJournalLocked(nil, nil); err != nil {
		t.Fatal(err)
	}
	journalPath := cfg.StatePath + ".journal"
	info, err = os.Stat(journalPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal mode=%v err=%v", info, err)
	}
	if err := p.clearJournalLocked(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(filepath.Dir(cfg.StatePath))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("state directory mode=%v err=%v", info, err)
	}
}

func TestSharedAuthDirectoryCanContainStatePath(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(root, "auth")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(authDir, 0775); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AuthDir: authDir, StatePath: filepath.Join(authDir, "state.json"), DefaultLimit: 1}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	info, err := os.Stat(authDir)
	if err != nil || info.Mode().Perm() != 0775 {
		t.Fatalf("shared state path changed auth directory mode: mode=%v err=%v", info, err)
	}
	for _, path := range []string{
		cfg.StatePath,
		filepath.Join(authDir, ".commandcode-pool.lock"),
		cfg.StatePath + ".lock",
		cfg.StatePath + ".journal.lock",
	} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("owned file %s mode=%v err=%v", path, info, err)
		}
	}
	if err := p.writeJournalLocked(nil, nil); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(cfg.StatePath + ".journal")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("shared-directory journal mode=%v err=%v", info, err)
	}
	if err := p.clearJournalLocked(); err != nil {
		t.Fatal(err)
	}
}

func TestAuthDirectoryRejectsSymlinkAndNonDirectory(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			base := filepath.Join(root, kind)
			if err := os.Mkdir(base, 0700); err != nil {
				t.Fatal(err)
			}
			authDir := filepath.Join(base, "auth")
			if kind == "symlink" {
				target := filepath.Join(base, "target")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, authDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(authDir, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{AuthDir: authDir, StatePath: filepath.Join(base, "private", "state.json")}
			if _, err := New(cfg); !errors.Is(err, ErrStorage) {
				t.Fatalf("auth root %s accepted: %v", kind, err)
			}
			if _, err := os.Stat(filepath.Dir(cfg.StatePath)); !os.IsNotExist(err) {
				t.Fatalf("state directory created after invalid auth root: %v", err)
			}
		})
	}
}

func TestRollbackFailureQuarantinesStorage(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "quarantine-storage-secret", "owner", 1)
	authPath := filepath.Join(cfg.AuthDir, a.AuthID)
	p.rename = func(src, dst string) error {
		if dst == cfg.StatePath {
			if err := os.Remove(authPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(authPath, 0700); err != nil {
				t.Fatal(err)
			}
			return errors.New("synthetic private failure")
		}
		return os.Rename(src, dst)
	}
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: "updated", GroupID: a.GroupID, MaxConcurrency: 1}); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	if view := findView(t, p, a.ID); view.Status != "storage_unconfirmed" {
		t.Fatal("rollback failure not quarantined")
	}
	if _, err := p.Acquire(a.AuthID, "req", "model"); !errors.Is(err, ErrStorage) {
		t.Fatal("unconfirmed storage admitted execution")
	}
	if _, err := p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: a.GroupID, MaxConcurrency: 1}); !errors.Is(err, ErrStorage) {
		t.Fatal("unconfirmed storage accepted write")
	}
}

func TestBoundedTerminalHistoryNeverDropsActiveOwner(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "bounded-terminal-secret", "owner", 1)
	lease, err := p.Acquire(a.AuthID, "active-request", "model")
	if err != nil {
		t.Fatal(err)
	}
	p.AbortRequest("active-request")
	for i := 0; i < 4200; i++ {
		p.AbortRequest(fmt.Sprintf("finished-%d", i))
	}
	p.mu.Lock()
	_, retained := p.terminal["active-request"]
	count := len(p.terminal)
	p.mu.Unlock()
	if !retained || count > 4096 {
		t.Fatal("active marker evicted or history unbounded")
	}
	if _, err := p.Acquire(a.AuthID, "active-request", "model"); !errors.Is(err, ErrAborted) {
		t.Fatal("late attempt admitted")
	}
	lease.Settle("done")
}

func TestDeletedActiveOwnerIdentityCannotBypassCap(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "identity-delete-old-key", "old-group", 1)
	q := Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Identity: "same-real-identity"}
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	lease, err := p.Acquire(a.AuthID, "old-request", "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	b := addAccount(t, p, "identity-delete-new-key", "new-group", 1)
	q.UpdatedAt = time.Now()
	if err := p.ObserveQuota(b.ID, q); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Acquire(b.AuthID, "new-request", "model"); !errors.Is(err, ErrDisabled) {
		t.Fatal("deleted active owner identity bypassed cap", err)
	}
	lease.Settle("done")
	next, err := p.Acquire(b.AuthID, "new-request", "model")
	if err != nil {
		t.Fatal("settled deleted owner unnecessarily blocks rotation", err)
	}
	next.Settle("done")
}

func TestStorageNamespaceCollisionRejected(t *testing.T) {
	dir := t.TempDir()
	for _, base := range []string{".commandcode-pool.lock", ProviderID + "-key-" + strings.Repeat("a", 64) + ".json"} {
		if _, err := New(Config{AuthDir: dir, StatePath: filepath.Join(dir, base)}); !errors.Is(err, ErrInvalid) {
			t.Fatal("colliding storage namespace accepted", err)
		}
	}
}

func TestActiveIdentityReplacementCannotReleaseOtherGroup(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "identity-active-key-a", "owner-a", 1)
	q := Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Identity: "identity-u"}
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	lease, err := p.Acquire(a.AuthID, "active-u", "model")
	if err != nil {
		t.Fatal(err)
	}
	b := addAccount(t, p, "identity-active-key-b", "owner-b", 1)
	if err := p.ObserveQuota(b.ID, q); err != nil {
		t.Fatal(err)
	}
	q.Identity = "identity-v"
	if err := p.ObserveQuota(a.ID, q); !errors.Is(err, ErrAdmission) {
		t.Fatal("active identity replaced", err)
	}
	if view := findView(t, p, a.ID); view.Quota.Identity != "identity-u" || view.Quota.Error == "" {
		t.Fatal("lost known active identity")
	}
	if _, err := p.Acquire(b.AuthID, "blocked-b", "model"); !errors.Is(err, ErrDisabled) {
		t.Fatal("old identity conflict bypassed", err)
	}
	lease.Settle("done")
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal("settled identity cannot update", err)
	}
	admitted, err := p.Acquire(b.AuthID, "now-b", "model")
	if err != nil {
		t.Fatal(err)
	}
	admitted.Settle("done")
}

func TestIdleMemberCannotSplitAnActiveGroup(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "active-group-a", "same-owner", 1)
	b := addAccount(t, p, "active-group-b", "same-owner", 1)
	owner, err := p.Acquire(a.AuthID, "active-a", "model")
	if err != nil {
		t.Fatal(err)
	}
	move := AccountInput{ID: b.ID, Name: b.Name, GroupID: "other-owner", MaxConcurrency: 1}
	if _, err := p.Upsert(move); !errors.Is(err, ErrInvalid) {
		t.Fatal("idle key split active group", err)
	}
	owner.Settle("done")
	if _, err := p.Upsert(move); err != nil {
		t.Fatal("settled group cannot move", err)
	}
}

func TestQuotaDisplayPointersAreDeepCopied(t *testing.T) {
	for _, boundary := range []string{"observation", "snapshot", "account clone"} {
		t.Run(boundary, func(t *testing.T) {
			p, _ := testPool(t, time.Minute)
			a := addAccount(t, p, "display-copy-synthetic-key", "owner", 1)
			monthly := 12.0
			periodEnd := time.Date(2026, 10, 16, 4, 3, 37, 0, time.UTC)
			q := Quota{Headroom: 0.2, RemainingCredits: 17, UpdatedAt: time.Now(),
				Month:          &Window{Name: "month", Source: "plan_allowance", Used: 288, Cap: 300, Remaining: 12},
				MonthlyCredits: &monthly, SubscriptionPeriodEnd: &periodEnd, SubscriptionStatus: "active",
				Windows: []Window{{Name: "weekly", Used: 80, Cap: 100, Remaining: 20}}}
			if err := p.ObserveQuota(a.ID, q); err != nil {
				t.Fatal(err)
			}
			copy := q
			switch boundary {
			case "snapshot":
				copy = findView(t, p, a.ID).Quota
			case "account clone":
				copy = cloneAccounts(p.accounts)[a.ID].quota
			}
			copy.Month.Remaining, copy.Month.Source = 999, "mutated"
			*copy.MonthlyCredits = 999
			*copy.SubscriptionPeriodEnd = time.Time{}
			copy.Windows[0].Remaining = 999
			stored := findView(t, p, a.ID).Quota
			if stored.Month == nil || stored.Month.Remaining != 12 || stored.Month.Source != "plan_allowance" ||
				stored.MonthlyCredits == nil || *stored.MonthlyCredits != 12 ||
				stored.SubscriptionPeriodEnd == nil || stored.SubscriptionPeriodEnd.Format(time.RFC3339) != "2026-10-16T04:03:37Z" ||
				stored.Windows[0].Remaining != 20 {
				t.Fatalf("%s retained shared data: %+v", boundary, stored)
			}
		})
	}
	q := cloneQuota(Quota{})
	if q.Month != nil || q.MonthlyCredits != nil || q.SubscriptionPeriodEnd != nil {
		t.Fatal("copy invented absent display metadata")
	}
}

func TestQuotaDisplayMetadataDoesNotAffectPickOrAcquire(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "display-routing-synthetic-key", "owner", 1)
	q := Quota{Headroom: 0.2, RemainingCredits: 17, UpdatedAt: time.Now()}
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	before := p.Pick([]string{a.AuthID}, "before-display", "model")
	lease, err := p.Acquire(a.AuthID, "before-display", "model")
	if err != nil {
		t.Fatal(err)
	}
	lease.Settle("done")
	monthly := 0.0
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	q.Month = &Window{Name: "month", Source: "plan_allowance", Used: 300, Cap: 300}
	q.MonthlyCredits, q.SubscriptionPeriodEnd, q.SubscriptionStatus = &monthly, &past, "canceled"
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	after := p.Pick([]string{a.AuthID}, "after-display", "model")
	if before.AuthID != after.AuthID || before.Reason != after.Reason || len(before.Candidates) != 1 || len(after.Candidates) != 1 || before.Candidates[0] != after.Candidates[0] {
		t.Fatalf("display metadata changed scores: before=%+v after=%+v", before, after)
	}
	lease, err = p.Acquire(a.AuthID, "after-display", "model")
	if err != nil {
		t.Fatalf("display-only zero monthly balance, past period end or canceled status blocked admission: %v", err)
	}
	lease.Settle("done")
	q.Headroom = 0
	q.Month.Remaining = 300
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	if decision := p.Pick([]string{a.AuthID}, "exhausted-window", "model"); decision.AuthID != "" {
		t.Fatal("display month overrode exhausted measured windows")
	}
	if _, err := p.Acquire(a.AuthID, "exhausted-window", "model"); !errors.Is(err, ErrQuotaUnavailable) {
		t.Fatalf("display month overrode exhausted admission: %v", err)
	}
}

func TestQuotaFailureClearsDisplayMetadataAndPreservesIdentity(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "display-failure-synthetic-key", "owner", 1)
	monthly := 12.0
	periodEnd := time.Now().UTC()
	q := Quota{Headroom: 0.2, RemainingCredits: 17, UpdatedAt: time.Now(), Identity: "org:known",
		Month:          &Window{Name: "month", Source: "plan_allowance", Cap: 300, Remaining: 12},
		MonthlyCredits: &monthly, SubscriptionPeriodEnd: &periodEnd, SubscriptionStatus: "active"}
	if err := p.ObserveQuota(a.ID, q); err != nil {
		t.Fatal(err)
	}
	q.Error = "synthetic failure"
	if err := p.ObserveQuota(a.ID, q); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	failed := findView(t, p, a.ID).Quota
	if failed.Error != "quota_unavailable" || failed.Identity != "org:known" || failed.Month != nil || failed.MonthlyCredits != nil || failed.SubscriptionPeriodEnd != nil || failed.SubscriptionStatus != "" {
		t.Fatalf("failure retained stale display data or lost identity: %+v", failed)
	}
}

func TestQuotaInvalidDisplayMetricsAreDroppedWithoutChangingAdmission(t *testing.T) {
	for _, field := range []string{"month used", "month cap", "month remaining", "monthly credits", "both"} {
		for _, invalid := range []struct {
			name  string
			value float64
		}{{"NaN", math.NaN()}, {"positive infinity", math.Inf(1)}, {"negative infinity", math.Inf(-1)}, {"negative", -1}} {
			t.Run(field+"/"+invalid.name, func(t *testing.T) {
				p, _ := testPool(t, time.Minute)
				a := addAccount(t, p, "invalid-display-synthetic-key", "owner", 1)
				monthly := 12.0
				periodEnd := time.Now().UTC()
				q := Quota{Headroom: 0.2, RemainingCredits: 17, UpdatedAt: time.Now(), Identity: "org:known",
					Month:          &Window{Name: "month", Source: "plan_allowance", Used: 288, Cap: 300, Remaining: 12},
					MonthlyCredits: &monthly, SubscriptionPeriodEnd: &periodEnd, SubscriptionStatus: "active",
					Windows: []Window{{Name: "weekly", Used: 80, Cap: 100, Remaining: 20}}}
				if err := p.ObserveQuota(a.ID, q); err != nil {
					t.Fatal(err)
				}
				before := p.Pick([]string{a.AuthID}, "before-invalid-display", "model")
				switch field {
				case "month used":
					q.Month.Used = invalid.value
				case "month cap":
					q.Month.Cap = invalid.value
				case "month remaining":
					q.Month.Remaining = invalid.value
				case "monthly credits":
					*q.MonthlyCredits = invalid.value
				case "both":
					q.Month.Remaining, *q.MonthlyCredits = invalid.value, invalid.value
				}
				if err := p.ObserveQuota(a.ID, q); err != nil {
					t.Fatalf("invalid optional display metadata rejected valid measured quota: %v", err)
				}
				stored := findView(t, p, a.ID).Quota
				if (stored.Month == nil) != (field != "monthly credits") || (stored.MonthlyCredits == nil) != (field == "monthly credits" || field == "both") {
					t.Fatalf("did not drop only invalid display fields: %+v", stored)
				}
				if stored.Error != "" || stored.RemainingCredits != 17 || stored.Headroom != 0.2 || stored.Identity != "org:known" || stored.SubscriptionStatus != "active" || stored.SubscriptionPeriodEnd == nil || *stored.SubscriptionPeriodEnd != periodEnd || len(stored.Windows) != 1 || stored.Windows[0] != q.Windows[0] {
					t.Fatalf("display sanitization changed trusted quota or other metadata: %+v", stored)
				}
				payload, err := json.Marshal(p.Snapshot())
				if err != nil || (stored.Month == nil && bytes.Contains(payload, []byte(`"month":`))) || (stored.MonthlyCredits == nil && bytes.Contains(payload, []byte(`"monthly_credits":`))) {
					t.Fatalf("snapshot JSON failed or retained invalid metadata: %s err=%v", payload, err)
				}
				after := p.Pick([]string{a.AuthID}, "after-invalid-display", "model")
				if before.AuthID != after.AuthID || before.Reason != after.Reason || len(before.Candidates) != 1 || len(after.Candidates) != 1 || before.Candidates[0] != after.Candidates[0] {
					t.Fatalf("invalid display metadata changed scores: before=%+v after=%+v", before, after)
				}
				lease, err := p.Acquire(a.AuthID, "after-invalid-display", "model")
				if err != nil {
					t.Fatalf("invalid display metadata changed admission: %v", err)
				}
				lease.Settle("done")
			})
		}
	}
}

func TestQuotaDisplayMetadataSecretProtection(t *testing.T) {
	for _, field := range []string{"month name", "month source", "window source", "subscription status"} {
		t.Run(field, func(t *testing.T) {
			p, _ := testPool(t, time.Minute)
			a := addAccount(t, p, "display-current-synthetic-key", "owner", 1)
			q := Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Month: &Window{Name: "month", Source: "plan_allowance", Cap: 30, Remaining: 10}, Windows: []Window{{Name: "weekly", Cap: 100, Remaining: 100}}}
			set := func(value string) {
				switch field {
				case "month name":
					q.Month.Name = value
				case "month source":
					q.Month.Source = value
				case "window source":
					q.Windows[0].Source = value
				case "subscription status":
					q.SubscriptionStatus = value
				}
			}
			futureKey := "display-future-synthetic-key"
			set("metadata " + futureKey)
			if err := p.ObserveQuota(a.ID, q); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Upsert(AccountInput{Name: "new", GroupID: "new-owner", APIKey: futureKey, MaxConcurrency: 1}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("new key matched %s metadata: %v", field, err)
			}
			for _, value := range []string{"metadata display-current-synthetic-key", strings.Repeat("x", 513)} {
				set(value)
				if err := p.ObserveQuota(a.ID, q); err != nil {
					t.Fatal(err)
				}
				payload, err := json.Marshal(p.Snapshot())
				if err != nil || bytes.Contains(payload, []byte(value)) || !bytes.Contains(payload, []byte("[redacted]")) {
					t.Fatalf("%s was not bounded/redacted: %s err=%v", field, payload, err)
				}
			}
			deletedKey := "display-deleted-synthetic-key"
			deleted := addAccount(t, p, deletedKey, "deleted-owner", 1)
			if err := p.Delete(deleted.ID); err != nil {
				t.Fatal(err)
			}
			set("metadata " + deletedKey)
			if err := p.ObserveQuota(a.ID, q); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(p.Snapshot())
			if err != nil || bytes.Contains(payload, []byte(deletedKey)) || !bytes.Contains(payload, []byte("[redacted]")) {
				t.Fatalf("%s leaked deleted secret: %s err=%v", field, payload, err)
			}
		})
	}
}

func TestMetadataSecretRejectionAndHistoricalEventRedaction(t *testing.T) {
	for _, field := range []string{"name", "group", "identity"} {
		t.Run(field, func(t *testing.T) {
			p, _ := testPool(t, time.Minute)
			futureKey := "future-synthetic-secret"
			a := addAccount(t, p, "current-synthetic-secret", "owner", 1)
			switch field {
			case "name":
				_, err := p.Upsert(AccountInput{ID: a.ID, Name: futureKey, GroupID: a.GroupID, MaxConcurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
			case "group":
				_, err := p.Upsert(AccountInput{ID: a.ID, Name: a.Name, GroupID: futureKey, MaxConcurrency: 1})
				if err != nil {
					t.Fatal(err)
				}
			case "identity":
				if err := p.ObserveQuota(a.ID, Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Identity: futureKey}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := p.Upsert(AccountInput{Name: "new", GroupID: "new-owner", APIKey: futureKey, MaxConcurrency: 1}); !errors.Is(err, ErrInvalid) {
				t.Fatal("new credential matched existing metadata", err)
			}
		})
	}
	p, cfg := testPool(t, time.Minute)
	key := "historical-synthetic-secret"
	p.Pick([]string{"candidate " + key}, "request "+key, "model "+key)
	a := addAccount(t, p, key, "owner", 1)
	if err := p.ObserveQuota(a.ID, Quota{Headroom: 1, RemainingCredits: 10, UpdatedAt: time.Now(), Identity: key}); !errors.Is(err, ErrInvalid) {
		t.Fatal("identity included credential", err)
	}
	payload, _ := json.Marshal(p.Events(0))
	if bytes.Contains(payload, []byte(key)) {
		t.Fatal("historical event leaked newly known secret")
	}
	if err := p.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, retained := p.secrets[key]; retained {
		t.Fatal("deleted raw key retained in secret map")
	}
	state, _ := os.ReadFile(cfg.StatePath)
	if bytes.Contains(state, []byte(key)) {
		t.Fatal("deleted secret persisted in metadata")
	}
	p.Close()
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.Upsert(AccountInput{Name: "name " + key, GroupID: "owner-two", APIKey: "another-synthetic-secret", MaxConcurrency: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatal("tombstone key accepted in new metadata", err)
	}
	restored.Pick([]string{"candidate " + key}, "request "+key, "model "+key)
	payload, _ = json.Marshal(restored.Events(0))
	if bytes.Contains(payload, []byte(key)) {
		t.Fatal("restarted tombstone secret leaked")
	}
}

func TestJournalRecoversCrashBeforeStateCommit(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "journal-current-key", "owner-a", 1)
	original, _ := os.ReadFile(cfg.StatePath)
	orphanKey := "journal-orphan-secret"
	orphanID := authID(keyHash(orphanKey))
	unrelated := filepath.Join(cfg.AuthDir, "unrelated-auth.json")
	if err := os.WriteFile(unrelated, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	p.rename = func(src, dst string) error {
		if dst == cfg.StatePath {
			panic("simulated process loss before state commit")
		}
		return os.Rename(src, dst)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated crash")
			}
		}()
		_, _ = p.Upsert(AccountInput{Name: "new", GroupID: "owner-b", APIKey: orphanKey, MaxConcurrency: 1})
	}()
	journal, err := os.ReadFile(p.journalPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(journal, []byte(orphanID)) || bytes.Contains(journal, []byte(orphanKey)) {
		t.Fatal("journal lacks identity or includes secret")
	}
	if _, err := os.Stat(filepath.Join(cfg.AuthDir, orphanID)); err != nil {
		t.Fatal("auth not committed before crash")
	}
	state, _ := os.ReadFile(cfg.StatePath)
	if !bytes.Equal(state, original) {
		t.Fatal("state unexpectedly committed")
	}
	p.Close()
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if len(restored.Snapshot()) != 1 || findView(t, restored, a.ID).GroupID != a.GroupID {
		t.Fatal("old state not recovered")
	}
	if _, err := os.Stat(filepath.Join(cfg.AuthDir, orphanID)); !os.IsNotExist(err) {
		t.Fatal("orphan secret auth not recovered")
	}
	if _, err := os.Stat(restored.journalPath()); !os.IsNotExist(err) {
		t.Fatal("recovered journal retained")
	}
	data, _ := os.ReadFile(unrelated)
	if string(data) != "untouched" {
		t.Fatal("unrelated auth touched")
	}
	if _, err := restored.Upsert(AccountInput{Name: "new", GroupID: "owner-b", APIKey: orphanKey, MaxConcurrency: 1}); err != nil {
		t.Fatal("recovered key cannot be added", err)
	}
}

func TestJournalValidatesAllPathsBeforeRecovery(t *testing.T) {
	for _, kind := range []string{"traversal", "duplicate", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			p, cfg := testPool(t, time.Minute)
			owned := authID(keyHash("recover-owned-secret"))
			orphan := filepath.Join(cfg.AuthDir, owned)
			if err := os.WriteFile(orphan, []byte("owned"), 0600); err != nil {
				t.Fatal(err)
			}
			ids := []string{owned}
			switch kind {
			case "traversal":
				ids = append(ids, "../outside.json")
			case "duplicate":
				ids = append(ids, owned)
			case "symlink":
				linked := authID(keyHash("recover-link-secret"))
				ids = append(ids, linked)
				target := filepath.Join(filepath.Dir(cfg.AuthDir), "outside-secret")
				if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(cfg.AuthDir, linked)); err != nil {
					t.Fatal(err)
				}
			}
			data, _ := json.Marshal(authJournal{Version: 1, NewAuthIDs: ids})
			if err := os.WriteFile(p.journalPath(), data, 0600); err != nil {
				t.Fatal(err)
			}
			p.Close()
			if _, err := New(cfg); !errors.Is(err, ErrStorage) {
				t.Fatal("invalid recovery accepted", err)
			}
			if data, err := os.ReadFile(orphan); err != nil || string(data) != "owned" {
				t.Fatal("validated after destructive recovery")
			}
		})
	}
}

func TestRepublishCatalogPersistsWithoutChangingAdmission(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	a := addAccount(t, p, "catalog-republish-secret-a", "owner", 1)
	b := addAccount(t, p, "catalog-republish-secret-b", "owner", 1)
	off := false
	if _, err := p.Upsert(AccountInput{ID: b.ID, Name: b.Name, GroupID: b.GroupID, MaxConcurrency: 1, Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	owner, err := p.Acquire(a.AuthID, "catalog-active", "model")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(cfg.AuthDir, a.AuthID))
	revision := strings.Repeat("a", 64)
	if err := p.RepublishCatalog(revision); err != nil {
		t.Fatal(err)
	}
	for _, acct := range []AccountView{a, b} {
		data, err := os.ReadFile(filepath.Join(cfg.AuthDir, acct.AuthID))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(`"catalog_revision":"`+revision+`"`)) {
			t.Fatal("auth revision missing")
		}
	}
	after, _ := os.ReadFile(filepath.Join(cfg.AuthDir, a.AuthID))
	if bytes.Equal(before, after) {
		t.Fatal("auth watcher cannot observe catalog change")
	}
	if findView(t, p, a.ID).Inflight != 1 || findView(t, p, b.ID).Enabled {
		t.Fatal("republish altered admission state")
	}
	for _, bad := range []string{"", strings.Repeat("a", 32), strings.Repeat("A", 64)} {
		if !errors.Is(p.RepublishCatalog(bad), ErrInvalid) {
			t.Fatal("bad revision accepted")
		}
	}
	owner.Settle("done")
	p.Close()
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if restored.accounts[a.ID].CatalogRevision != revision || restored.accounts[b.ID].CatalogRevision != revision {
		t.Fatal("catalog revision not restored")
	}
	if c, _ := restored.Credential(b.AuthID); c.Enabled {
		t.Fatal("disabled credential revived")
	}
}

// Real subprocess exit prevents deferred cleanup from disguising a crash.
func TestOwnedStagingRecoveredAfterAbruptExit(t *testing.T) {
	const helperEnv = "COMMANDCODE_POOL_CRASH_TEST_DIR"
	const key = "abrupt-exit-synthetic-secret"
	if dir := os.Getenv(helperEnv); dir != "" {
		cfg := Config{AuthDir: filepath.Join(dir, "auth"), StatePath: filepath.Join(dir, "private", "state.json")}
		p, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		id := authID(keyHash(key))
		mode := os.Getenv("COMMANDCODE_POOL_CRASH_TEST_MODE")
		if err := p.writeJournalLocked([]string{id}, []string{id}); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "auth":
			_, err = stageOwnedFile(filepath.Join(cfg.AuthDir, id), []byte(key))
		case "state":
			_, err = stageOwnedFile(cfg.StatePath, []byte(key))
		case "rollback":
			_, err = stageOwnedFile(filepath.Join(cfg.AuthDir, id), []byte(key))
		case "journal":
			if err = p.clearJournalLocked(); err == nil {
				_, err = stageOwnedFile(p.journalPath(), []byte(`{"version":1}`))
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(23)
	}
	for _, mode := range []string{"auth", "state", "rollback", "journal"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestOwnedStagingRecoveredAfterAbruptExit$")
			cmd.Env = append(os.Environ(), helperEnv+"="+dir, "COMMANDCODE_POOL_CRASH_TEST_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("crash helper did not exit at stage: %v %s", err, output)
			}
			cfg := Config{AuthDir: filepath.Join(dir, "auth"), StatePath: filepath.Join(dir, "private", "state.json")}
			// An unrelated canonical-looking staging file is not ours to sweep.
			unrelated := filepath.Join(cfg.AuthDir, ".commandcode-pool-stage-unrelated.tmp")
			if err := os.WriteFile(unrelated, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			id := authID(keyHash(key))
			for _, path := range []string{ownedStagePath(cfg.StatePath), ownedStagePath(p.journalPath()), ownedStagePath(filepath.Join(cfg.AuthDir, id))} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("secret staging survived recovery", path, err)
				}
			}
			if data, err := os.ReadFile(unrelated); err != nil || string(data) != "untouched" {
				t.Fatal("unrelated staging touched")
			}
		})
	}
}

func TestStagingSymlinkRejectedBeforeAnyRecoveryDeletion(t *testing.T) {
	p, cfg := testPool(t, time.Minute)
	id := authID(keyHash("stage-symlink-secret"))
	orphan := filepath.Join(cfg.AuthDir, id)
	if err := os.WriteFile(orphan, []byte("preserve orphan until validation"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(cfg.AuthDir), "external.txt")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ownedStagePath(orphan)); err != nil {
		t.Fatal(err)
	}
	if err := p.writeJournalLocked([]string{id}, []string{id}); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, err := New(cfg); !errors.Is(err, ErrStorage) {
		t.Fatal("staging symlink accepted", err)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("orphan deleted before complete validation")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "preserve" {
		t.Fatal("staging symlink target overwritten")
	}
	if _, err := stageOwnedFile(orphan, []byte("unsafe")); err == nil {
		t.Fatal("stage writer followed symlink")
	}
}
