// Package pool owns admission and secret storage. Storage is single-process:
// Unix flock is held until every acquired upstream owner has settled, including
// after Close. Quotas and runtime leases deliberately do not survive restart.
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid           = errors.New("invalid account pool input")
	ErrStorage           = errors.New("account pool storage unavailable")
	ErrAdmission         = errors.New("account pool admission denied")
	ErrClosed            = errors.New("account pool closed")
	ErrUnknownCredential = errors.New("unknown pool credential")
	ErrDisabled          = errors.New("pool credential disabled")
	ErrAtCapacity        = errors.New("pool account at capacity")
	ErrQuotaUnavailable  = errors.New("pool quota unavailable")
	ErrAborted           = errors.New("pool request already finished")
)

type account struct {
	ID              string `json:"id"`
	AuthID          string `json:"auth_id"`
	Name            string `json:"name"`
	GroupID         string `json:"group_id"`
	APIKey          string `json:"api_key,omitempty"`
	Fingerprint     string `json:"fingerprint"`
	KeyLength       int    `json:"key_length,omitempty"`
	Limit           int    `json:"max_concurrency"`
	Enabled         bool   `json:"enabled"`
	Deleted         bool   `json:"deleted,omitempty"`
	Identity        string `json:"identity,omitempty"`
	CatalogRevision string `json:"catalog_revision,omitempty"`
	quota           Quota
}

type Pool struct {
	mu            sync.Mutex
	cfg           Config
	accounts      map[string]*account
	attempts      map[string]*Lease
	inflight      map[string]int
	terminal      map[string]struct{}
	terminalOrder []string
	events        []Event
	requestEvents []Event
	sequence      uint64
	nextAttempt   uint64
	tie           uint64
	// roundRobinLast is the auth ID chosen by the previous round-robin pick. The
	// eligible set changes between requests, so rotation advances by identity
	// rather than by an index into a set whose size moves.
	roundRobinLast string
	// sticky maps a session key to the account it was pinned to.
	sticky map[string]stickyBinding
	closed         bool
	storageBroken  bool
	locks          []*storageLock
	secrets        map[string]struct{}
	rename         func(string, string) error // instance-local transaction failure test seam
}

// Lease is an upstream owner's reservation, not an expiring ticket. Cancel only
// cancels Context; only the owner calling Settle proves upstream I/O is over.
type Lease struct {
	Credential  Credential
	AttemptID   string
	Context     context.Context
	pool        *Pool
	cancel      context.CancelFunc
	requestID   string
	traceID     string
	model       string
	groupID     string
	accountID   string
	settled     bool
	quarantined bool
}

// Scheduling strategies. The values are the canonical configuration spellings.
const (
	StrategyHeadroomFirst = "headroom-first"
	StrategyRoundRobin    = "round-robin"
	StrategyFillFirst     = "fill-first"
	StrategyExpiryFirst   = "expiry-first"
)

// ValidStrategy reports whether s is a canonical strategy spelling.
func ValidStrategy(s string) bool {
	switch s {
	case StrategyHeadroomFirst, StrategyRoundRobin, StrategyFillFirst, StrategyExpiryFirst:
		return true
	}
	return false
}

// Sticky states recorded on a decision or event. They describe only whether a
// session binding was honoured; the session key itself is never recorded.
const (
	StickyOff      = "off"
	StickyHit      = "hit"
	StickyMiss     = "miss"
	StickyFallback = "fallback"
)

// MaxStickyTTL bounds sticky sessions so a stale binding cannot outlive an
// operator's intent; MaxStickySessions bounds the in-process table.
const (
	MaxStickyTTL      = 168 * time.Hour
	MaxStickySessions = 4096
)

type stickyBinding struct {
	authID  string
	expires time.Time
}

// SetStickyTTL enables (positive) or disables (zero) sticky sessions on a
// running pool. Sessions bound while the TTL was longer keep their own expiry.
func (p *Pool) SetStickyTTL(ttl time.Duration) error {
	if ttl < 0 || ttl > MaxStickyTTL {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.StickyTTL = ttl
	if ttl == 0 {
		p.sticky = make(map[string]stickyBinding)
	}
	return nil
}

// StickyTTL returns the effective sticky session window; zero means disabled.
func (p *Pool) StickyTTL() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.StickyTTL
}

// bindStickyLocked records a fresh binding. Existing keys are always allowed to
// rebind — refusing them at capacity would leave a session stuck on an account
// it can no longer use. Only brand-new keys are subject to the table limit.
func (p *Pool) bindStickyLocked(key, authID string, now time.Time) {
	if key == "" || p.cfg.StickyTTL <= 0 {
		return
	}
	expires := now.Add(p.cfg.StickyTTL)
	if _, exists := p.sticky[key]; exists {
		p.sticky[key] = stickyBinding{authID: authID, expires: expires}
		return
	}
	if len(p.sticky) >= MaxStickySessions {
		for existing, binding := range p.sticky {
			if !binding.expires.After(now) {
				delete(p.sticky, existing)
			}
		}
		if len(p.sticky) >= MaxStickySessions {
			return // 表满且无过期项：拒绝新键
		}
	}
	p.sticky[key] = stickyBinding{authID: authID, expires: expires}
}

// normalizeStrategy maps blank or unknown values to the default strategy. It is
// only for constructor input; runtime changes go through SetStrategy, which
// rejects unknown values instead of silently falling back.
func normalizeStrategy(s string) string {
	if ValidStrategy(s) {
		return s
	}
	return StrategyHeadroomFirst
}

// SetStrategy switches the scheduling strategy of a running pool. Only affects
// later picks: scheduling never reserves, so in-flight leases are untouched.
func (p *Pool) SetStrategy(strategy string) error {
	if !ValidStrategy(strategy) {
		return ErrInvalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.Strategy = strategy
	return nil
}

// Strategy returns the effective scheduling strategy.
func (p *Pool) Strategy() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg.Strategy
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func authID(hash string) string { return ProviderID + "-key-" + hash + ".json" }

func (p *Pool) Upsert(in AccountInput) (AccountView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return AccountView{}, ErrClosed
	}
	if p.storageBroken {
		return AccountView{}, ErrStorage
	}
	in.ID, in.Name, in.GroupID = strings.TrimSpace(in.ID), strings.TrimSpace(in.Name), strings.TrimSpace(in.GroupID)
	in.APIKey = strings.TrimSpace(in.APIKey)
	if in.Name == "" || in.GroupID == "" || len(in.Name) > 256 || len(in.GroupID) > 256 || len(in.APIKey) > 4096 || p.containsSecretLocked(in.Name) || p.containsSecretLocked(in.GroupID) {
		return AccountView{}, ErrInvalid
	}
	if in.APIKey != "" {
		if strings.Contains(in.Name, in.APIKey) || strings.Contains(in.GroupID, in.APIKey) {
			return AccountView{}, ErrInvalid
		}
		for _, existing := range p.accounts {
			if strings.Contains(existing.Name, in.APIKey) || strings.Contains(existing.GroupID, in.APIKey) || strings.Contains(existing.Identity, in.APIKey) || strings.Contains(existing.CatalogRevision, in.APIKey) {
				return AccountView{}, ErrInvalid
			}
		}
	}
	limit := in.MaxConcurrency
	if limit == 0 {
		limit = p.cfg.DefaultLimit
	}
	if limit < 1 || limit > 1000 {
		return AccountView{}, ErrInvalid
	}
	old := p.accounts[in.ID]
	if in.ID != "" && old == nil {
		return AccountView{}, ErrInvalid
	}
	if in.APIKey != "" {
		fingerprint := keyHash(in.APIKey)
		for _, a := range p.accounts {
			if a.Fingerprint == fingerprint {
				if old != nil && old.ID != a.ID {
					return AccountView{}, ErrInvalid
				}
				old = a
				break
			}
		}
		if old != nil && old.Fingerprint != fingerprint {
			return AccountView{}, ErrInvalid
		} // delete + add for rotation
	}
	if old != nil && old.Deleted {
		return AccountView{}, ErrInvalid
	}
	if old == nil && in.APIKey == "" {
		return AccountView{}, ErrInvalid
	}
	if old == nil {
		// Display metadata can echo an already registered key. Preserve same-key
		// updates/imports, but never admit a new key overlapping that metadata.
		for _, existing := range p.accounts {
			if strings.Contains(existing.quota.SubscriptionStatus, in.APIKey) {
				return AccountView{}, ErrInvalid
			}
			if month := existing.quota.Month; month != nil && (strings.Contains(month.Name, in.APIKey) || strings.Contains(month.Source, in.APIKey)) {
				return AccountView{}, ErrInvalid
			}
			for _, window := range existing.quota.Windows {
				if strings.Contains(window.Source, in.APIKey) {
					return AccountView{}, ErrInvalid
				}
			}
		}
	}
	if old != nil && old.GroupID != in.GroupID {
		if p.inflight[old.GroupID] > 0 || (in.ID == "" && in.APIKey != "") {
			return AccountView{}, ErrInvalid
		}
	}
	// Adding a key must not silently change an existing group's cap.
	for _, a := range p.accounts {
		if !a.Deleted && a.GroupID == in.GroupID && (old == nil || old.GroupID != in.GroupID) && a.Limit != limit {
			return AccountView{}, ErrInvalid
		}
	}
	if p.inflight[in.GroupID] > limit {
		return AccountView{}, ErrAdmission
	}
	next := cloneAccounts(p.accounts)
	var a *account
	if old == nil {
		fingerprint := keyHash(in.APIKey)
		a = &account{ID: fingerprint, AuthID: authID(fingerprint), APIKey: in.APIKey, Fingerprint: fingerprint, KeyLength: len(in.APIKey), Enabled: true}
		// Even a caller-chosen ID must never collide with another credential.
		if _, exists := next[a.ID]; exists {
			return AccountView{}, ErrInvalid
		}
	} else {
		a = next[old.ID]
	}
	a.Name, a.GroupID, a.Limit = in.Name, in.GroupID, limit
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	next[a.ID] = a
	for _, member := range next {
		if !member.Deleted && member.GroupID == a.GroupID {
			member.Limit = limit
		}
	}
	if err := p.persistLocked(next); err != nil {
		return AccountView{}, err
	}
	p.accounts = next
	if a.APIKey != "" {
		p.secrets[a.APIKey] = struct{}{}
	}
	p.appendLocked(Event{AccountID: a.ID, GroupID: a.GroupID, Action: "account_updated", Reason: "account_updated"})
	return p.viewLocked(a, time.Now()), nil
}

// RepublishCatalog forces CPA's auth watcher to observe successfully refreshed
// model metadata without changing credentials, group limits, or live leases.
func (p *Pool) RepublishCatalog(revision string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.storageBroken {
		return ErrStorage
	}
	if !validFingerprint(revision) || p.containsSecretLocked(revision) {
		return ErrInvalid
	}
	next := cloneAccounts(p.accounts)
	for _, a := range next {
		if !a.Deleted {
			a.CatalogRevision = revision
		}
	}
	if err := p.persistLocked(next); err != nil {
		return err
	}
	p.accounts = next
	return nil
}

// Delete removes the secret and auth file, but retains the key fingerprint and
// auth identity. Legacy configured-key bootstrap can therefore never revive it.
func (p *Pool) Delete(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if p.storageBroken {
		return ErrStorage
	}
	old := p.accounts[id]
	if old == nil {
		return ErrInvalid
	}
	if old.Deleted {
		return nil
	}
	next := cloneAccounts(p.accounts)
	next[id].Enabled, next[id].Deleted, next[id].APIKey = false, true, ""
	next[id].quota = Quota{}
	if err := p.persistLocked(next); err != nil {
		return err
	}
	p.accounts = next
	delete(p.secrets, old.APIKey)
	for _, lease := range p.attempts {
		if lease.accountID == id {
			lease.cancel()
		}
	}
	p.appendLocked(Event{AccountID: id, GroupID: old.GroupID, Action: "account_deleted", Reason: "account_deleted"})
	return nil
}

func cloneQuota(q Quota) Quota {
	q.Windows = append([]Window(nil), q.Windows...)
	if q.Month != nil {
		month := *q.Month
		q.Month = &month
	}
	if q.MonthlyCredits != nil {
		monthly := *q.MonthlyCredits
		q.MonthlyCredits = &monthly
	}
	if q.SubscriptionPeriodEnd != nil {
		periodEnd := *q.SubscriptionPeriodEnd
		q.SubscriptionPeriodEnd = &periodEnd
	}
	return q
}

func cloneAccounts(src map[string]*account) map[string]*account {
	out := make(map[string]*account, len(src))
	for id, a := range src {
		copy := *a
		copy.quota = cloneQuota(a.quota)
		out[id] = &copy
	}
	return out
}
func (p *Pool) credentialLocked(a *account) Credential {
	return Credential{ID: a.ID, AuthID: a.AuthID, Name: a.Name, GroupID: a.GroupID, APIKey: a.APIKey, Enabled: a.Enabled && !a.Deleted}
}
func (p *Pool) Credential(id string) (Credential, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.AuthID == id {
			return p.credentialLocked(a), true
		}
	}
	return Credential{}, false
}
func (p *Pool) Credentials() []Credential {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Credential, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, p.credentialLocked(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out
}
func (p *Pool) Snapshot() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]AccountView, 0, len(p.accounts))
	for _, a := range p.accounts {
		if !a.Deleted {
			out = append(out, p.viewLocked(a, now))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (p *Pool) viewLocked(a *account, now time.Time) AccountView {
	q := cloneQuota(a.quota)
	if q.Identity == "" {
		q.Identity = a.Identity
	}
	q.Email = p.redactLocked(q.Email)
	q.Plan, q.Identity = p.redactLocked(q.Plan), p.redactLocked(q.Identity)
	q.SubscriptionStatus = p.redactLocked(q.SubscriptionStatus)
	if q.Month != nil {
		q.Month.Name = p.redactLocked(q.Month.Name)
		q.Month.Source = p.redactLocked(q.Month.Source)
	}
	for i := range q.Windows {
		q.Windows[i].Name = p.redactLocked(q.Windows[i].Name)
		q.Windows[i].Source = p.redactLocked(q.Windows[i].Source)
	}
	return AccountView{ID: p.redactLocked(a.ID), AuthID: a.AuthID, Name: p.redactLocked(a.Name), GroupID: p.redactLocked(a.GroupID), MaxConcurrency: a.Limit, Enabled: a.Enabled, Inflight: p.inflight[a.GroupID], KeyFingerprint: a.Fingerprint, Quota: q, Status: p.reasonLocked(a, now)}
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
func validQuota(q Quota) bool {
	if !finite(q.Headroom) || !finite(q.RemainingCredits) || q.UpdatedAt.IsZero() || q.UpdatedAt.After(time.Now()) {
		return false
	}
	for _, w := range q.Windows {
		if !finite(w.Used) || !finite(w.Cap) || !finite(w.Remaining) {
			return false
		}
	}
	return true
}

// ObserveQuota accepts only measured finite snapshots. A failed observation
// immediately invalidates the old snapshot, rather than retaining stale credit.
func (p *Pool) ObserveQuota(id string, q Quota) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	a := p.accounts[id]
	if a == nil || a.Deleted {
		return ErrInvalid
	}
	failed := Quota{Error: "quota_unavailable", Identity: a.Identity, UpdatedAt: q.UpdatedAt}
	if failed.UpdatedAt.IsZero() || failed.UpdatedAt.After(time.Now()) {
		failed.UpdatedAt = time.Now()
	}
	if q.Error != "" || !validQuota(q) {
		a.quota = failed
		p.appendLocked(Event{AccountID: a.ID, GroupID: a.GroupID, Action: "quota_observed", Reason: "quota_unavailable"})
		return ErrInvalid
	}
	// Invalid optional display metrics must not poison a valid measured quota
	// or prevent JSON output. Drop only the affected display field.
	if month := q.Month; month != nil && (!finite(month.Used) || !finite(month.Cap) || !finite(month.Remaining)) {
		q.Month = nil
	}
	if q.MonthlyCredits != nil && !finite(*q.MonthlyCredits) {
		q.MonthlyCredits = nil
	}
	q = cloneQuota(q)
	q.Identity = strings.TrimSpace(q.Identity)
	if len(q.Identity) > 256 || p.containsSecretLocked(q.Identity) {
		a.quota = failed
		return ErrInvalid
	}
	if q.Identity != "" && q.Identity != a.Identity {
		if a.Identity != "" && p.accountActiveLocked(a.ID) {
			a.quota = failed
			return ErrAdmission
		}
		next := cloneAccounts(p.accounts)
		next[id].Identity = q.Identity
		if err := p.persistLocked(next); err != nil {
			a.quota = failed
			return err
		}
		p.accounts = next
		a = next[id]
	}
	if q.Identity == "" {
		q.Identity = a.Identity
	}
	a.quota = q
	p.appendLocked(Event{AccountID: a.ID, GroupID: a.GroupID, Action: "quota_observed", Reason: p.reasonLocked(a, time.Now())})
	return nil
}
func (p *Pool) identityConflictLocked(a *account) bool {
	if a.Identity == "" {
		return false
	}
	for _, other := range p.accounts {
		if (!other.Deleted || p.accountActiveLocked(other.ID)) && other.Identity == a.Identity && other.GroupID != a.GroupID {
			return true
		}
	}
	return false
}
func (p *Pool) accountActiveLocked(id string) bool {
	for _, lease := range p.attempts {
		if lease.accountID == id {
			return true
		}
	}
	return false
}
func (p *Pool) reasonLocked(a *account, now time.Time) string {
	if p.closed {
		return "closed"
	}
	if p.storageBroken {
		return "storage_unconfirmed"
	}
	for _, lease := range p.attempts {
		if lease.groupID == a.GroupID && lease.quarantined {
			return "cleanup_unconfirmed"
		}
	}
	if a.Deleted {
		return "deleted"
	}
	if !a.Enabled || a.APIKey == "" {
		return "disabled"
	}
	if p.identityConflictLocked(a) {
		return "identity_conflict"
	}
	q := a.quota
	if q.Error != "" {
		return "quota_unavailable"
	}
	if q.UpdatedAt.IsZero() || q.UpdatedAt.After(now) || now.Sub(q.UpdatedAt) > p.cfg.QuotaMaxAge {
		return "quota_stale"
	}
	if q.RemainingCredits <= 0 || q.Headroom <= 0 {
		return "quota_exhausted"
	}
	if p.inflight[a.GroupID] >= a.Limit {
		return "concurrency_limit"
	}
	return "eligible"
}

func (p *Pool) Pick(ids []string, requestID, model string) Decision {
	return p.PickWithTrace(ids, requestID, model, "")
}

func (p *Pool) PickWithTrace(ids []string, requestID, model, traceID string) Decision {
	return p.PickWithSession(ids, requestID, model, traceID, "")
}

// PickWithSession selects one eligible account. A non-empty stickyKey pins that
// session to the account it picked first for the configured window, as long as
// that account stays eligible; otherwise the configured strategy decides and the
// binding is replaced.
func (p *Pool) PickWithSession(ids []string, requestID, model, traceID, stickyKey string) Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	decision := Decision{Reason: "no_eligible_account", Candidates: make([]CandidateScore, 0, len(ids))}
	// Expiry-first reads the subscription display fields, which deliberately
	// stay out of CandidateScore so display metadata never perturbs scores.
	var expiry map[string]expiration
	if p.cfg.Strategy == StrategyExpiryFirst {
		expiry = make(map[string]expiration, len(ids))
	}
	var best []CandidateScore
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		score := CandidateScore{AuthID: p.redactLocked(id), Reason: "unknown_auth"}
		for _, a := range p.accounts {
			if a.AuthID != id {
				continue
			}
			score = CandidateScore{AccountID: p.redactLocked(a.ID), AuthID: a.AuthID, GroupID: p.redactLocked(a.GroupID), Headroom: a.quota.Headroom, RemainingCredits: a.quota.RemainingCredits, Inflight: p.inflight[a.GroupID], Limit: a.Limit, Reason: p.reasonLocked(a, now)}
			if strings.TrimSpace(requestID) == "" {
				score.Reason = "missing_request_id"
			} else if _, done := p.terminal[requestID]; done {
				score.Reason = "request_finished"
			}
			score.Eligible = score.Reason == "eligible"
			if expiry != nil {
				expiry[a.AuthID] = expirationOf(a.quota)
			}
			break
		}
		decision.Candidates = append(decision.Candidates, score)
		if score.Eligible {
			best = append(best, score)
		}
	}
	sort.Slice(decision.Candidates, func(i, j int) bool { return decision.Candidates[i].AuthID < decision.Candidates[j].AuthID })
	key := strings.TrimSpace(stickyKey)
	stickyState := StickyOff
	var selected CandidateScore
	ok := false
	if key != "" && p.cfg.StickyTTL > 0 {
		binding, bound := p.sticky[key]
		if bound && !binding.expires.After(now) {
			delete(p.sticky, key)
			bound = false
			stickyState = StickyFallback
		}
		if bound {
			for _, candidate := range best {
				if candidate.AuthID == binding.authID {
					selected, ok, stickyState = candidate, true, StickyHit
					break
				}
			}
			if !ok {
				stickyState = StickyFallback
			}
		} else if stickyState != StickyFallback {
			stickyState = StickyMiss
		}
	}
	if !ok {
		if selected, ok = p.selectLocked(best, expiry, now); ok {
			p.tie++
			p.bindStickyLocked(key, selected.AuthID, now)
		}
	} else if binding, bound := p.sticky[key]; bound {
		// Slide the window while the session stays active.
		binding.expires = now.Add(p.cfg.StickyTTL)
		p.sticky[key] = binding
	}
	if ok {
		decision.AuthID, decision.AccountID, decision.Reason = selected.AuthID, selected.AccountID, "selected"
	}
	decision.Strategy = p.cfg.Strategy
	decision.Sticky = stickyState
	p.appendLocked(Event{RequestID: requestID, TraceID: traceID, Model: model, AccountID: decision.AccountID, Action: "pick", Reason: decision.Reason, Strategy: p.cfg.Strategy, Sticky: stickyState, Candidates: decision.Candidates})
	return decision
}

// headroomLess is the backstop ordering shared by headroom-first and the
// expiry-first fallback: most headroom, then most credits, then least load.
func headroomLess(a, b CandidateScore) bool {
	if a.Headroom != b.Headroom {
		return a.Headroom > b.Headroom
	}
	if a.RemainingCredits != b.RemainingCredits {
		return a.RemainingCredits > b.RemainingCredits
	}
	if a.Inflight*b.Limit != b.Inflight*a.Limit {
		return a.Inflight*b.Limit < b.Inflight*a.Limit
	}
	return a.AuthID < b.AuthID
}

// expiration carries the subscription inputs expiry-first reads. It stays out
// of CandidateScore so display metadata cannot perturb admission or scores.
type expiration struct {
	periodEnd time.Time
	hasEnd    bool
	monthly   float64
	hasMon    bool
}

func expirationOf(q Quota) expiration {
	e := expiration{}
	if q.SubscriptionPeriodEnd != nil {
		e.periodEnd, e.hasEnd = *q.SubscriptionPeriodEnd, true
	}
	if q.MonthlyCredits != nil {
		e.monthly, e.hasMon = *q.MonthlyCredits, true
	}
	return e
}

// rotateTop returns the candidate at the rotation offset within the leading run
// of equal-ranked entries, so identical candidates share load evenly.
func rotateTop(ranked []CandidateScore, equal func(a, b CandidateScore) bool, tie uint64) CandidateScore {
	count := 1
	for count < len(ranked) && equal(ranked[0], ranked[count]) {
		count++
	}
	return ranked[tie%uint64(count)]
}

// selectLocked applies the configured strategy to the eligible candidates. It
// reorders best in place; callers must not depend on the input order.
func (p *Pool) selectLocked(best []CandidateScore, expiry map[string]expiration, now time.Time) (CandidateScore, bool) {
	if len(best) == 0 {
		return CandidateScore{}, false
	}
	switch p.cfg.Strategy {
	case StrategyRoundRobin:
		// Rotate across every eligible candidate, ignoring scores: even
		// consumption at the cost of a higher chance of hitting a limit. The
		// cursor advances to the next auth ID after the previous pick, so an
		// account that is always eligible cannot be starved by peers that come
		// and go.
		sort.SliceStable(best, func(i, j int) bool { return best[i].AuthID < best[j].AuthID })
		idx := sort.Search(len(best), func(i int) bool { return best[i].AuthID > p.roundRobinLast })
		if idx == len(best) {
			idx = 0
		}
		selected := best[idx]
		p.roundRobinLast = selected.AuthID
		return selected, true
	case StrategyFillFirst:
		// Fixed order, exhaust the current account before moving on.
		sort.SliceStable(best, func(i, j int) bool { return best[i].AuthID < best[j].AuthID })
		return best[0], true
	case StrategyExpiryFirst:
		// Priority only for a future period end that still carries monthly
		// credits: an exhausted monthly allowance has nothing left to save by
		// resetting, so it must not outrank an account that does.
		tier := make([]CandidateScore, 0, len(best))
		for _, c := range best {
			if e := expiry[c.AuthID]; e.hasEnd && e.periodEnd.After(now) && e.hasMon && e.monthly > 0 {
				tier = append(tier, c)
			}
		}
		if len(tier) > 0 {
			sort.SliceStable(tier, func(i, j int) bool {
				ei, ej := expiry[tier[i].AuthID], expiry[tier[j].AuthID]
				if !ei.periodEnd.Equal(ej.periodEnd) {
					return ei.periodEnd.Before(ej.periodEnd)
				}
				return headroomLess(tier[i], tier[j])
			})
			return rotateTop(tier, func(a, b CandidateScore) bool {
				return expiry[a.AuthID].periodEnd.Equal(expiry[b.AuthID].periodEnd) && sameScore(a, b)
			}, p.tie), true
		}
	}
	// headroom-first default, and the expiry-first backstop.
	sort.SliceStable(best, func(i, j int) bool { return headroomLess(best[i], best[j]) })
	return rotateTop(best, sameScore, p.tie), true
}
func sameScore(a, b CandidateScore) bool {
	return a.Headroom == b.Headroom && a.RemainingCredits == b.RemainingCredits && a.Inflight*b.Limit == b.Inflight*a.Limit
}

func (p *Pool) Acquire(id, requestID, model string) (*Lease, error) {
	return p.AcquireWithTrace(id, requestID, model, "")
}

func (p *Pool) AcquireWithTrace(id, requestID, model, traceID string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.TrimSpace(requestID) == "" {
		return nil, ErrAdmission
	}
	if p.closed {
		return nil, ErrClosed
	}
	if p.storageBroken {
		return nil, ErrStorage
	}
	if _, done := p.terminal[requestID]; done {
		return nil, ErrAborted
	}
	var a *account
	for _, candidate := range p.accounts {
		if candidate.AuthID == id {
			a = candidate
			break
		}
	}
	if a == nil {
		return nil, ErrUnknownCredential
	}
	reason := p.reasonLocked(a, time.Now())
	if reason != "eligible" {
		p.appendLocked(Event{RequestID: requestID, TraceID: traceID, Model: model, AccountID: a.ID, GroupID: a.GroupID, Action: "acquire_rejected", Reason: reason})
		switch reason {
		case "disabled", "deleted", "identity_conflict":
			return nil, ErrDisabled
		case "concurrency_limit", "cleanup_unconfirmed":
			return nil, ErrAtCapacity
		case "quota_stale", "quota_exhausted", "quota_unavailable":
			return nil, ErrQuotaUnavailable
		default:
			return nil, ErrAdmission
		}
	}
	p.nextAttempt++
	ctx, cancel := context.WithCancel(context.Background())
	lease := &Lease{Credential: p.credentialLocked(a), AttemptID: attemptID(p.nextAttempt), Context: ctx, pool: p, cancel: cancel, requestID: requestID, traceID: NormalizeTraceID(traceID), model: model, groupID: a.GroupID, accountID: a.ID}
	p.attempts[lease.AttemptID] = lease
	p.inflight[a.GroupID]++
	p.appendLocked(Event{RequestID: requestID, TraceID: lease.traceID, AttemptID: lease.AttemptID, Model: model, AccountID: a.ID, GroupID: a.GroupID, Action: "acquire", Reason: "acquired"})
	return lease, nil
}
func (l *Lease) Cancel() {
	if l != nil && l.cancel != nil {
		l.cancel()
	}
}
func (l *Lease) Record(reason string) {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.settled {
		return
	}
	l.quarantined = true
	p.appendLocked(Event{RequestID: l.requestID, TraceID: l.traceID, AttemptID: l.AttemptID, Model: l.model, AccountID: l.accountID, GroupID: l.groupID, Action: "quarantined", Reason: safeOwnerReason(reason, "cleanup_unconfirmed")})
}
func (l *Lease) Settle(reason string) {
	if l == nil || l.pool == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.settled {
		return
	}
	l.settled = true
	l.cancel()
	delete(p.attempts, l.AttemptID)
	p.inflight[l.groupID]--
	p.trimTerminalLocked()
	p.appendLocked(Event{RequestID: l.requestID, TraceID: l.traceID, AttemptID: l.AttemptID, Model: l.model, AccountID: l.accountID, GroupID: l.groupID, Action: "settle", Reason: safeOwnerReason(reason, "owner_finished")})
	if p.closed && len(p.attempts) == 0 {
		p.releaseLocksLocked()
	}
}

// Owner diagnostics are codes, never upstream errors or payloads. Unknown
// caller text is replaced rather than echoed, even when it has no known key.
func safeOwnerReason(reason, fallback string) string {
	switch reason {
	case "invalid_execution_credential", "execution_validation_failed",
		"upstream_cleanup_unconfirmed", "upstream_network_failure",
		"upstream_status_failure", "upstream_translation_failure",
		"upstream_complete", "upstream_stream_failure", "upstream_producer_panicked",
		"upstream_canceled", "upstream_response_limit", "downstream_emit_failure",
		"upstream_read_failure", "cleanup_unconfirmed", "owner_finished",
		"done", "complete", "completed", "cancelled", "canceled", "confirmed_cleanup":
		return reason
	default:
		return fallback
	}
}

// AbortRequest is a bounded terminal marker plus cancellation, never release.
// Active requests are not evicted; unrelated completed IDs retain a recent
// bounded history to reject late RPC attempts without an unbounded registry.
func (p *Pool) AbortRequest(id string) {
	p.AbortRequestWithTrace(id, "")
}

func (p *Pool) AbortRequestWithTrace(id, traceID string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.terminal[id]; !exists {
		p.terminal[id] = struct{}{}
		p.terminalOrder = append(p.terminalOrder, id)
	}
	for _, lease := range p.attempts {
		if lease.requestID == id {
			lease.cancel()
		}
	}
	p.trimTerminalLocked()
	p.appendLocked(Event{RequestID: id, TraceID: traceID, Action: "request_aborted", Reason: "request_finished"})
}
func (p *Pool) trimTerminalLocked() {
	const limit = 4096
	for len(p.terminalOrder) > limit {
		index := -1
		for i, id := range p.terminalOrder {
			active := false
			for _, lease := range p.attempts {
				if lease.requestID == id {
					active = true
					break
				}
			}
			if !active {
				index = i
				break
			}
		}
		if index < 0 {
			return
		}
		delete(p.terminal, p.terminalOrder[index])
		p.terminalOrder = append(p.terminalOrder[:index], p.terminalOrder[index+1:]...)
	}
}
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for _, lease := range p.attempts {
		lease.cancel()
	}
	if len(p.attempts) == 0 {
		p.releaseLocksLocked()
	}
}
func (p *Pool) containsSecretLocked(text string) bool {
	if len(text) > 512 {
		return true
	}
	for key := range p.secrets {
		if key != "" && strings.Contains(text, key) {
			return true
		}
	}
	for _, a := range p.accounts {
		if !a.Deleted {
			continue
		}
		first, last := a.KeyLength, a.KeyLength
		if first == 0 {
			first, last = 1, len(text)
		}
		for length := first; length <= last; length++ {
			for start := 0; start+length <= len(text); start++ {
				if keyHash(text[start:start+length]) == a.Fingerprint {
					return true
				}
			}
		}
	}
	return false
}

func (p *Pool) redactLocked(text string) string {
	// Diagnostics are bounded; legacy tombstones without KeyLength require a
	// bounded scan of possible substring lengths, never unbounded payload work.
	if len(text) > 512 {
		return "[redacted]"
	}
	for key := range p.secrets {
		if key != "" {
			text = strings.ReplaceAll(text, key, "[redacted]")
		}
	}
	for _, a := range p.accounts {
		if !a.Deleted {
			continue
		}
		first, last := a.KeyLength, a.KeyLength
		if first == 0 {
			first, last = 1, len(text)
		} // legacy tombstone migration
		for length := first; length <= last; length++ {
			for start := 0; start+length <= len(text); {
				if keyHash(text[start:start+length]) == a.Fingerprint {
					text = text[:start] + "[redacted]" + text[start+length:]
					start += len("[redacted]")
				} else {
					start++
				}
			}
		}
	}
	return text
}
func (p *Pool) appendLocked(e Event) {
	p.sequence++
	e.Sequence = p.sequence
	e.At = time.Now()
	e.RequestID = p.redactLocked(e.RequestID)
	e.TraceID = NormalizeTraceID(p.redactLocked(NormalizeTraceID(e.TraceID)))
	e.Model = p.redactLocked(e.Model)
	e.Reason = p.redactLocked(e.Reason)
	e.AccountID = p.redactLocked(e.AccountID)
	e.GroupID = p.redactLocked(e.GroupID)
	e.Candidates = append([]CandidateScore(nil), e.Candidates...)
	p.events = appendEvent(p.events, e, p.cfg.EventLimit)
	if e.RequestID != "" && e.Model != "" {
		switch e.Action {
		case "pick", "acquire", "acquire_rejected", "settle", "quarantined":
			p.requestEvents = appendEvent(p.requestEvents, e, p.cfg.EventLimit)
		}
	}
}
func appendEvent(events []Event, e Event, limit int) []Event {
	if len(events) == limit {
		copy(events, events[1:])
		events[len(events)-1] = e
		return events
	}
	return append(events, e)
}
func (p *Pool) Events(after uint64) []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.eventsAfterLocked(p.events, after)
}
func (p *Pool) RequestEvents(after uint64) []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.eventsAfterLocked(p.requestEvents, after)
}
func (p *Pool) eventsAfterLocked(events []Event, after uint64) []Event {
	out := make([]Event, 0)
	for _, e := range events {
		if e.Sequence > after {
			copy := e
			copy.RequestID = p.redactLocked(copy.RequestID)
			copy.TraceID = NormalizeTraceID(p.redactLocked(copy.TraceID))
			copy.Model = p.redactLocked(copy.Model)
			copy.Reason = p.redactLocked(copy.Reason)
			copy.AccountID = p.redactLocked(copy.AccountID)
			copy.GroupID = p.redactLocked(copy.GroupID)
			copy.Candidates = append([]CandidateScore(nil), e.Candidates...)
			for i := range copy.Candidates {
				candidate := &copy.Candidates[i]
				candidate.AccountID = p.redactLocked(candidate.AccountID)
				candidate.AuthID = p.redactLocked(candidate.AuthID)
				candidate.GroupID = p.redactLocked(candidate.GroupID)
				candidate.Reason = p.redactLocked(candidate.Reason)
			}
			out = append(out, copy)
		}
	}
	return out
}
