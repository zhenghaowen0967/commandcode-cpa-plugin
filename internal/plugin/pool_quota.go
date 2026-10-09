package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/pool"
)

const (
	poolQuotaDefaultInterval = time.Minute
	poolQuotaWorkers         = 4
	poolQuotaMaxBody         = int64(1 << 20)
)

// All errors crossing the management boundary are fixed strings: transport
// errors may contain proxy credentials, URLs, or an upstream's echoed key.
var (
	errPoolQuotaUnavailable = errors.New("account pool unavailable")
	errPoolQuotaUnknown     = errors.New("unknown account")
	errPoolQuotaCanceled    = errors.New("account quota refresh canceled")
	errPoolQuotaFailed      = errors.New("account quota refresh failed")
	errPoolQuotaBase        = errors.New("account API base unavailable")
	errPoolQuotaRequest     = errors.New("account request failed")
	errPoolQuotaProxy       = errors.New("account proxy configuration unavailable")
	errPoolQuotaRejected    = errors.New("account request rejected")
	errPoolQuotaBody        = errors.New("account response too large")
	errPoolQuotaInvalid     = errors.New("account credits response invalid")
)

func newPoolQuotaTransport(proxyURL string) (*http.Transport, error) {
	proxy, err := config.ProxyFunc(proxyURL)
	if err != nil {
		return nil, errPoolQuotaProxy
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		return nil, errPoolQuotaProxy
	}
	transport := defaultTransport.Clone()
	transport.Proxy = proxy
	return transport, nil
}

// refreshPoolQuotas serializes manual/background probes, but waiting for that
// ownership is cancellable. A credential worker issues only one HTTP request
// at a time, so four workers are also a hard bound of four in-flight requests.
func (m *Manager) refreshPoolQuotas(ctx context.Context, id string) error {
	if err := m.lockPoolQuotaProbe(ctx); err != nil {
		return err
	}
	defer m.probeMu.Unlock()

	m.mu.RLock()
	p, baseURL, timeout, proxyURL := m.pool, m.cfg.BaseURL, m.cfg.RequestTimeout, m.cfg.ProxyURL
	m.mu.RUnlock()
	if p == nil {
		return errPoolQuotaUnavailable
	}
	credentials := p.Credentials()
	selected := make([]pool.Credential, 0, len(credentials))
	for _, credential := range credentials {
		if id != "" {
			if credential.ID == id {
				selected = append(selected, credential)
				break
			}
		} else if credential.Enabled {
			selected = append(selected, credential)
		}
	}
	if id != "" && len(selected) == 0 {
		return errPoolQuotaUnknown
	}
	if len(selected) == 0 {
		return nil // No parsing, bridge traffic, or HTTP for an empty pool.
	}
	base, err := AccountAPIBase(baseURL)
	if err != nil {
		for _, credential := range selected {
			p.ObserveQuota(credential.ID, pool.Quota{Error: errPoolQuotaBase.Error(), UpdatedAt: time.Now()})
		}
		return errPoolQuotaBase
	}
	if timeout <= 0 || timeout > quotaMaxTimeout {
		timeout = quotaMaxTimeout
	}
	transport, err := newPoolQuotaTransport(proxyURL)
	if err != nil {
		for _, credential := range selected {
			p.ObserveQuota(credential.ID, pool.Quota{Error: errPoolQuotaProxy.Error(), UpdatedAt: time.Now()})
		}
		return errPoolQuotaFailed
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errPoolQuotaRejected // Never forward a key, even to the same host.
		},
	}
	defer transport.CloseIdleConnections()

	jobs := make(chan pool.Credential)
	var wg sync.WaitGroup
	var failuresMu sync.Mutex
	failed := false
	workers := min(poolQuotaWorkers, len(selected))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for credential := range jobs {
				if ctx.Err() != nil {
					return
				}
				quota, probeErr := fetchPoolQuota(ctx, client, base, timeout, credential.APIKey)
				p.ObserveQuota(credential.ID, quota)
				if probeErr != nil {
					failuresMu.Lock()
					failed = true
					failuresMu.Unlock()
				}
			}
		}()
	}
queue:
	for _, credential := range selected {
		select {
		case <-ctx.Done():
			break queue
		case jobs <- credential:
		}
	}
	close(jobs)
	wg.Wait() // No request-owning goroutine survives return or loop stop.
	if ctx.Err() != nil {
		return errPoolQuotaCanceled
	}
	if failed {
		return errPoolQuotaFailed
	}
	return nil
}

func (m *Manager) lockPoolQuotaProbe(ctx context.Context) error {
	if ctx.Err() != nil {
		return errPoolQuotaCanceled
	}
	if m.probeMu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errPoolQuotaCanceled
		case <-ticker.C:
			if m.probeMu.TryLock() {
				if ctx.Err() != nil {
					m.probeMu.Unlock()
					return errPoolQuotaCanceled
				}
				return nil
			}
		}
	}
}

// startPoolQuotaLoop probes immediately, then defaults to a sixty-second
// cadence. Lifecycle code must stop (cancel + join) before clearing m.pool.
func (m *Manager) startPoolQuotaLoop(interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = poolQuotaDefaultInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return
			}
			_ = m.refreshPoolQuotas(ctx, "")
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// Pointer metrics distinguish absent/null from an actual zero balance or usage.
// A missing window is not evidence of unlimited allowance: its headroom is zero.
type poolAccountCredits struct {
	Credits *struct {
		Monthly   *float64 `json:"monthlyCredits"`
		Purchased *float64 `json:"purchasedCredits"`
		Free      *float64 `json:"freeCredits"`
	} `json:"credits"`
	WindowLimits *struct {
		Limited  *bool              `json:"limited"`
		Exceeded json.RawMessage    `json:"exceeded"`
		FiveHour *poolAccountWindow `json:"fiveHour"`
		Weekly   *poolAccountWindow `json:"weekly"`
	} `json:"windowLimits"`
}

type poolAccountWindow struct {
	Used     *float64        `json:"used"`
	Cap      *float64        `json:"cap"`
	Exceeded json.RawMessage `json:"exceeded"`
	ResetAt  *int64          `json:"resetAt"`
}

func parsePoolCredits(raw []byte) (pool.Quota, error) {
	var credits poolAccountCredits
	if json.Unmarshal(raw, &credits) != nil || credits.Credits == nil {
		return pool.Quota{}, errPoolQuotaInvalid
	}
	var quota pool.Quota
	for _, value := range []*float64{credits.Credits.Monthly, credits.Credits.Purchased, credits.Credits.Free} {
		if value == nil || !finiteNonnegative(*value) {
			return pool.Quota{}, errPoolQuotaInvalid
		}
		quota.RemainingCredits += *value
	}
	if !finiteNonnegative(quota.RemainingCredits) {
		return pool.Quota{}, errPoolQuotaInvalid
	}
	monthly := *credits.Credits.Monthly
	quota.MonthlyCredits = &monthly
	if credits.WindowLimits == nil {
		return quota, nil
	}
	limits := credits.WindowLimits
	exceeded, valid := poolExceeded(limits.Exceeded)
	if !valid {
		return pool.Quota{}, errPoolQuotaInvalid
	}
	// limited indicates that rate limiting is enabled, not that capacity
	// is exhausted. Only actual exceeded flags or measured usage block it.
	blocked := exceeded
	for _, named := range []struct {
		name   string
		window *poolAccountWindow
	}{{"five_hour", limits.FiveHour}, {"weekly", limits.Weekly}} {
		window := named.window
		if window == nil {
			continue
		}
		if window.Used == nil || window.Cap == nil || !finiteNonnegative(*window.Used) || !finiteNonnegative(*window.Cap) || *window.Cap <= 0 {
			return pool.Quota{}, errPoolQuotaInvalid
		}
		windowExceeded, valid := poolExceeded(window.Exceeded)
		if !valid {
			return pool.Quota{}, errPoolQuotaInvalid
		}
		blocked = blocked || windowExceeded
		remaining := math.Max(0, *window.Cap-*window.Used)
		headroom := math.Max(0, math.Min(1, remaining / *window.Cap))
		if len(quota.Windows) == 0 || headroom < quota.Headroom {
			quota.Headroom = headroom
		}
		out := pool.Window{Name: named.name, Used: *window.Used, Cap: *window.Cap, Remaining: remaining}
		if window.ResetAt != nil && *window.ResetAt > 0 {
			out.ResetAt = time.UnixMilli(*window.ResetAt).UTC()
		}
		quota.Windows = append(quota.Windows, out)
	}
	if blocked {
		quota.Headroom = 0
	}
	return quota, nil
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

// The vendor reports global exceeded as null, false, or an exceeded window's
// name. Unrecognized shapes fail closed instead of silently granting capacity.
func poolExceeded(raw json.RawMessage) (exceeded, valid bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, true
	}
	var flag bool
	if json.Unmarshal(raw, &flag) == nil {
		return flag, true
	}
	var name string
	if json.Unmarshal(raw, &name) == nil {
		return name != "", true
	}
	return false, false
}

func fetchPoolQuota(ctx context.Context, client *http.Client, base string, timeout time.Duration, key string) (pool.Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	failure := func(err error) (pool.Quota, error) {
		return pool.Quota{UpdatedAt: time.Now(), Error: err.Error()}, err
	}
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, errPoolQuotaRequest
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "cli")
		resp, err := client.Do(req)
		if err != nil {
			return nil, errPoolQuotaRequest
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, errPoolQuotaRejected
		}
		if resp.ContentLength > poolQuotaMaxBody {
			return nil, errPoolQuotaBody
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, poolQuotaMaxBody+1))
		if err != nil {
			return nil, errPoolQuotaRequest
		}
		if int64(len(raw)) > poolQuotaMaxBody {
			return nil, errPoolQuotaBody
		}
		return raw, nil
	}
	raw, err := get(accountCreditsPath)
	if err != nil {
		return failure(err)
	}
	quota, err := parsePoolCredits(raw)
	if err != nil {
		return failure(err)
	}
	quota.UpdatedAt = time.Now()
	// Metadata failures never turn a real successful credits reading into an
	// unknown quota. Pool.ObserveQuota retains a previously known identity.
	if raw, err := get(accountSubscriptionPath); err == nil {
		var sub accountSubscription
		if json.Unmarshal(raw, &sub) == nil && sub.Success {
			var allowance float64
			quota.Plan, allowance = planFor(sub.Data.PlanID) // Display only, not routing.
			if quota.Plan == "" {
				quota.Plan = strings.TrimSpace(sub.Data.PlanID)
			}
			if allowance > 0 && quota.MonthlyCredits != nil {
				// The denominator is a static plan allowance, not an upstream total.
				// Purchased/free credits belong only in RemainingCredits.
				remaining := *quota.MonthlyCredits
				cap := math.Max(allowance, remaining)
				quota.Month = &pool.Window{Name: "month", Source: "plan_allowance", Used: math.Max(0, cap-remaining), Cap: cap, Remaining: remaining}
			}
			quota.SubscriptionStatus = strings.TrimSpace(sub.Data.Status)
			// A subscription period end is not evidence of a monthly reset or
			// permanent expiry. Reuse the legacy date normalization only.
			if periodEnd, err := time.Parse(time.RFC3339, periodEndRFC3339(sub.Data.CurrentPeriodEnd)); err == nil {
				quota.SubscriptionPeriodEnd = &periodEnd
			}
		}
	}
	if raw, err := get(accountWhoamiPath); err == nil {
		var who struct {
			Success *bool `json:"success"`
			Org     *struct {
				ID json.RawMessage `json:"id"`
			} `json:"org"`
			User struct {
				ID    json.RawMessage `json:"id"`
				Email string          `json:"email"`
			} `json:"user"`
		}
		if json.Unmarshal(raw, &who) == nil && (who.Success == nil || *who.Success) {
			quota.Email = strings.TrimSpace(who.User.Email)
			if who.Org != nil {
				if identity := poolStableID(who.Org.ID); identity != "" {
					quota.Identity = "org:" + identity
				}
			}
			if quota.Identity == "" {
				if identity := poolStableID(who.User.ID); identity != "" {
					quota.Identity = "user:" + identity
				}
			}
		}
	}
	return quota, nil
}

func poolStableID(raw json.RawMessage) string {
	var id string
	if json.Unmarshal(raw, &id) == nil {
		return strings.TrimSpace(id)
	}
	// Some account surfaces use integer IDs. Preserve their exact spelling;
	// floating/exponent values, booleans, names, emails and logins are not IDs.
	if _, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return string(raw)
	}
	return ""
}
