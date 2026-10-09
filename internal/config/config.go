// Package config loads and validates the commandcode plugin configuration
// per spec 04 (configuration) and spec 05 §2 (HTTPS/timeouts).
package config

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults (spec 04 §2/§4).
const (
	DefaultBaseURL          = "https://api.commandcode.ai/provider/v1"
	DefaultModelPrefix      = "commandcode"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
)

type ModelPrefix struct {
	Enabled bool
	Value   string
}

type APIKey struct {
	Value string
}

type Catalog struct {
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Protocols struct {
	ChatCompletions bool
	Messages        bool
	Responses       bool
}

type RouteOverride struct {
	Protocol string `yaml:"protocol"`
	Endpoint string `yaml:"endpoint"`
}

type PoolConfig struct {
	AuthDir              string
	StatePath            string
	DefaultLimit         int
	QuotaMaxAge          time.Duration
	QuotaRefreshInterval time.Duration
}

type Config struct {
	Pool             PoolConfig
	GuardOnly        bool
	BaseURL          string
	CatalogURL       string
	ProxyURL         string
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        Protocols
	RouteOverrides   map[string]RouteOverride
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawPool struct {
	AuthDir              string  `yaml:"auth-dir"`
	StatePath            string  `yaml:"state-path"`
	DefaultLimit         *int    `yaml:"max-concurrency"`
	QuotaMaxAge          *string `yaml:"quota-max-age"`
	QuotaRefreshInterval *string `yaml:"quota-refresh-interval"`
}

type rawConfig struct {
	Pool             rawPool                  `yaml:"pool"`
	GuardOnly        bool                     `yaml:"guard-only"`
	BaseURL          *string                  `yaml:"base-url"`
	CatalogURL       *string                  `yaml:"catalog-url"`
	ProxyURL         *string                  `yaml:"proxy-url"`
	ModelPrefix      rawPrefix                `yaml:"model-prefix"`
	APIKeys          []rawKey                 `yaml:"api-keys"`
	Catalog          rawCatalog               `yaml:"catalog"`
	Protocols        rawProtocols             `yaml:"protocols"`
	RouteOverrides   map[string]RouteOverride `yaml:"route-overrides"`
	AllowHTTP        bool                     `yaml:"allow-http"`
	RequestTimeout   *string                  `yaml:"request-timeout"`
	MaxResponseBytes *int64                   `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Value string `yaml:"value"`
}

type rawCatalog struct {
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

type rawProtocols struct {
	ChatCompletions *bool `yaml:"chat-completions"`
	Messages        *bool `yaml:"messages"`
	Responses       *bool `yaml:"responses"`
}

var (
	validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}
)

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates (spec 04 §6). Decode errors never echo
// decoded node values — a malformed entry (e.g. a bare-scalar API key)
// must not leak into the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		keys[i] = APIKey{Value: os.ExpandEnv(k.Value)}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	quotaMaxAge, err := parseDuration("pool.quota-max-age", raw.Pool.QuotaMaxAge, 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	quotaRefresh, err := parseDuration("pool.quota-refresh-interval", raw.Pool.QuotaRefreshInterval, time.Minute)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Pool: PoolConfig{
			AuthDir: strings.TrimSpace(raw.Pool.AuthDir), StatePath: strings.TrimSpace(raw.Pool.StatePath),
			DefaultLimit: orDefault(raw.Pool.DefaultLimit, 2), QuotaMaxAge: quotaMaxAge,
			QuotaRefreshInterval: quotaRefresh,
		},
		GuardOnly: raw.GuardOnly,
		BaseURL:   orDefault(raw.BaseURL, DefaultBaseURL),
		ProxyURL:  strings.TrimSpace(orDefault(raw.ProxyURL, "")),
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: Protocols{
			ChatCompletions: orDefault(raw.Protocols.ChatCompletions, true),
			Messages:        orDefault(raw.Protocols.Messages, true),
			Responses:       orDefault(raw.Protocols.Responses, true),
		},
		RouteOverrides:   raw.RouteOverrides,
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if raw.CatalogURL != nil {
		// Mirror the derived-default trim so an explicit trailing-slash
		// catalog-url cannot double up separators downstream.
		c.CatalogURL = strings.TrimRight(*raw.CatalogURL, "/")
	} else {
		c.CatalogURL = strings.TrimRight(c.BaseURL, "/") + "/models"
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// PublicID returns the client-facing model ID (FR-003).
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if c.GuardOnly && len(c.APIKeys) > 0 {
		return fmt.Errorf("guard-only: api-keys must be empty")
	}
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateURL("catalog-url", c.CatalogURL, c.AllowHTTP); err != nil {
		return err
	}
	if err := validateProxyURL(c.ProxyURL); err != nil {
		return err
	}
	if c.Pool.DefaultLimit < 1 || c.Pool.DefaultLimit > 1000 {
		return fmt.Errorf("pool.max-concurrency: must be between 1 and 1000")
	}
	if c.Pool.QuotaMaxAge <= 0 {
		return fmt.Errorf("pool.quota-max-age: must be positive")
	}
	if c.Pool.QuotaRefreshInterval < time.Second {
		return fmt.Errorf("pool.quota-refresh-interval: must be at least 1s")
	}
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for _, k := range c.APIKeys {
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	for name, o := range c.RouteOverrides {
		if !validProtocols[o.Protocol] {
			return fmt.Errorf("route-overrides[%s].protocol: unsupported protocol %q", name, o.Protocol)
		}
		if o.Endpoint == "" {
			return fmt.Errorf("route-overrides[%s].endpoint: must not be empty", name)
		}
		if !strings.HasPrefix(o.Endpoint, "/") {
			return fmt.Errorf("route-overrides[%s].endpoint: must start with /", name)
		}
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	return nil
}

func validateProxyURL(raw string) error {
	_, err := ProxyFunc(raw)
	return err
}

// ProxyFunc converts CPA's proxy-url value into the stdlib transport policy.
// An empty value inherits environment proxy settings; direct/none disables them.
// The standard transport supports HTTP, HTTPS, SOCKS5, and SOCKS5H proxies.
func ProxyFunc(raw string) (func(*http.Request) (*url.URL, error), error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return http.ProxyFromEnvironment, nil
	}
	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		return nil, nil
	}
	proxyURL, err := url.Parse(trimmed)
	if err != nil || proxyURL.Scheme == "" || proxyURL.Host == "" || strings.TrimSpace(proxyURL.Hostname()) == "" {
		return nil, fmt.Errorf("proxy-url: invalid proxy configuration")
	}
	switch proxyURL.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("proxy-url: invalid proxy configuration")
	}
	if port := proxyURL.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("proxy-url: invalid proxy configuration")
		}
	}
	return http.ProxyURL(proxyURL), nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	// Scheme allowlist (spec 05 §2): https always; http only behind
	// allow-http; anything else (ftp://, file://, custom schemes) is
	// rejected at load instead of failing at transport time.
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}
