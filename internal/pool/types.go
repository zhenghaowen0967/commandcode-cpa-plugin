package pool

import "time"

const ProviderID = "commandcode-pool"

// Config keeps secret state outside source and generated page resources.
type Config struct {
	AuthDir      string
	StatePath    string
	QuotaMaxAge  time.Duration
	EventLimit   int
	DefaultLimit int
}

type AccountInput struct {
	ID             string `json:"id,omitempty"`
	Name           string `json:"name"`
	GroupID        string `json:"group_id"`
	APIKey         string `json:"api_key,omitempty"`
	MaxConcurrency int    `json:"max_concurrency"`
	Enabled        *bool  `json:"enabled,omitempty"`
}

type Credential struct {
	ID      string
	AuthID  string
	Name    string
	GroupID string
	APIKey  string
	Enabled bool
}

type Window struct {
	Name      string    `json:"name"`
	Used      float64   `json:"used"`
	Cap       float64   `json:"cap"`
	Remaining float64   `json:"remaining"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
	Source    string    `json:"source,omitempty"`
}

type Quota struct {
	RemainingCredits float64   `json:"remaining_credits"`
	Headroom         float64   `json:"headroom"`
	Windows          []Window  `json:"windows"`
	UpdatedAt        time.Time `json:"updated_at"`
	Error            string    `json:"error,omitempty"`
	Email            string    `json:"email,omitempty"`
	Plan             string    `json:"plan,omitempty"`
	Identity         string    `json:"identity,omitempty"`
	// Display-only metadata; Month is not a rate-limit window or routing input.
	Month                 *Window    `json:"month,omitempty"`
	MonthlyCredits        *float64   `json:"monthly_credits,omitempty"`
	SubscriptionPeriodEnd *time.Time `json:"subscription_period_end,omitempty"`
	SubscriptionStatus    string     `json:"subscription_status,omitempty"`
}

type AccountView struct {
	ID             string `json:"id"`
	AuthID         string `json:"auth_id"`
	Name           string `json:"name"`
	GroupID        string `json:"group_id"`
	MaxConcurrency int    `json:"max_concurrency"`
	Enabled        bool   `json:"enabled"`
	Inflight       int    `json:"inflight"`
	KeyFingerprint string `json:"key_fingerprint"`
	Quota          Quota  `json:"quota"`
	Status         string `json:"status"`
}

type CandidateScore struct {
	AccountID        string  `json:"account_id"`
	AuthID           string  `json:"auth_id"`
	GroupID          string  `json:"group_id"`
	Headroom         float64 `json:"headroom"`
	RemainingCredits float64 `json:"remaining_credits"`
	Inflight         int     `json:"inflight"`
	Limit            int     `json:"limit"`
	Eligible         bool    `json:"eligible"`
	Reason           string  `json:"reason"`
}

type Decision struct {
	AuthID     string           `json:"auth_id,omitempty"`
	AccountID  string           `json:"account_id,omitempty"`
	Reason     string           `json:"reason"`
	Candidates []CandidateScore `json:"candidates"`
}

type Event struct {
	Sequence   uint64           `json:"sequence"`
	At         time.Time        `json:"at"`
	RequestID  string           `json:"request_id"`
	AttemptID  string           `json:"attempt_id,omitempty"`
	Model      string           `json:"model"`
	AccountID  string           `json:"account_id,omitempty"`
	GroupID    string           `json:"group_id,omitempty"`
	Action     string           `json:"action"`
	Reason     string           `json:"reason"`
	Candidates []CandidateScore `json:"candidates,omitempty"`
}
