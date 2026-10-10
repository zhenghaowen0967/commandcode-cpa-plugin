package plugin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/catalog"
	"commandcode-cpa-plugin/internal/codexguard"
	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/errclass"
	"commandcode-cpa-plugin/internal/pool"
)

const ProviderID = pool.ProviderID

const (
	originalPluginID  = "commandcode-pool"
	nextPluginID      = "commandcode-pool-next"
	updatePluginID    = "commandcode-pool-update"
	pluginVersion     = "0.1.9-local"
	viewPluginVersion = "0.1.1-view"
)

var pluginName = originalPluginID

// pluginRole is selected at link time by scripts/build.sh. The view role is a
// static-page compatibility shim and must never enter the full dispatcher.
var pluginRole = "full"

// 本地安装版本不指向基座的发行包，防止自动更新覆盖本插件。
const githubRepoURL = "https://github.com/zhenghaowen0967/commandcode-cpa-plugin"

// registerRefreshTimeout bounds ONLY the synchronous initial/reconfigure
// refreshOnce so a slow catalog cannot block host startup/reconfigure for a
// full request-timeout (default 15m). On expiry registration proceeds per
// FR-002 empty/stale semantics; the ticker retries at refresh-interval with
// the full request-timeout.
const registerRefreshTimeout = 10 * time.Second

// Manager owns dispatcher state (config snapshot, catalog manager, refresh
// loop) and routes every RPC method. Safe for concurrent HandleCall use.
type Manager struct {
	bridge *HostBridge // immutable after NewManager

	// lifeMu serializes whole register/reconfigure/shutdown sequences so
	// their stop-wait-install steps cannot interleave into orphaned tickers.
	lifeMu sync.Mutex

	mu  sync.RWMutex
	cfg config.Config
	mgr *catalog.Manager
	// stop/done manage the one background refresh goroutine; both nil
	// when no loop is running.
	stop chan struct{}
	done chan struct{}

	codex                codexguard.Guard
	pool                 *pool.Pool
	poolAuthDir          string
	poolStatePath        string
	poolQuotaStop        func()
	probeMu              sync.Mutex
	catalogMu            sync.Mutex
	executionWG          sync.WaitGroup
	managementWG         sync.WaitGroup
	execClosing          atomic.Bool
	usageEvents          atomic.Uint64
	poolActivationError  string
	hasValidRegistration bool
}

// NewManager returns a dispatcher whose outbound traffic flows through bridge.
func NewManager(bridge *HostBridge) *Manager {
	return &Manager{bridge: bridge}
}

func (m *Manager) UsageEventsSeen() uint64 {
	return m.usageEvents.Load()
}

// ActivationError 仅返回固定的脱敏状态值。
func (m *Manager) ActivationError() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.poolActivationError
}

func (m *Manager) guardOnly() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.GuardOnly
}

const poolActivationFailureMessage = "pool activation failed"

func (m *Manager) lifecycleFailureEnvelope(code, message string) []byte {
	m.mu.Lock()
	if m.hasValidRegistration {
		guardOnly := m.cfg.GuardOnly
		m.poolActivationError = poolActivationFailureMessage
		m.mu.Unlock()
		return registrationEnvelope(guardOnly)
	}
	m.mu.Unlock()
	return ErrEnvelope(code, message)
}

func managementResourcePage(page string) []byte {
	return managementResourcePageForPlugin(page, pluginName)
}

func managementResourcePageForPlugin(page, targetPluginID string) []byte {
	return []byte(strings.ReplaceAll(page, "/v0/management/plugins/"+originalPluginID, "/v0/management/plugins/"+targetPluginID))
}

// HandleCall dispatches one RPC method and returns envelope bytes. Handler
// failures travel inside the envelope; a recovered panic becomes a
// "plugin_error" envelope so the host process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	debugTrace("handle method=%s request_bytes=%d", method, len(request))
	defer func() {
		// Defensive: host-boundary panics are goroutine-contained (see
		// HostBridge.callWithTimeout); this guards future handler bugs.
		if r := recover(); r != nil {
			resp = ErrEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	if pluginRole == "view" {
		switch method {
		case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
			return m.handleViewLifecycle(request)
		case pluginabi.MethodPluginShutdown:
			return m.handleViewShutdown()
		case pluginabi.MethodManagementRegister:
			return m.registerViewManagement(request)
		case pluginabi.MethodManagementHandle:
			return m.handleViewManagement(request)
		default:
			return ErrEnvelope("unsupported", "method unavailable in compatibility view mode"), nil
		}
	}
	if m.guardOnly() {
		switch method {
		case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
			return m.handleLifecycle(request)
		case pluginabi.MethodPluginShutdown:
			return m.handleShutdown()
		case pluginabi.MethodSchedulerPick:
			return m.handleGuardOnlySchedulerPick(request)
		case pluginabi.MethodUsageHandle:
			return m.handleCodexUsage(request)
		case pluginabi.MethodManagementRegister:
			return m.registerManagement(request)
		case pluginabi.MethodManagementHandle:
			return m.handleManagement(request)
		default:
			return ErrEnvelope("unsupported", "method unavailable in guard-only mode"), nil
		}
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return m.handleLifecycle(request)
	case pluginabi.MethodModelStatic:
		return m.handleModels()
	case pluginabi.MethodModelForAuth:
		return m.handleModelsForAuth(request)
	case pluginabi.MethodPluginShutdown:
		return m.handleShutdown()
	case pluginabi.MethodExecutorExecute:
		return m.handleExecute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return m.handleExecuteStream(request)
	case pluginabi.MethodAuthIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth parse request body"), nil
		}
		resp, err := (authProvider{}).ParseAuth(context.Background(), req)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		debugTrace("auth parse response provider=%s id=%s attr_api_key_present=%t storage_json_bytes=%d", resp.Auth.Provider, resp.Auth.ID, strings.TrimSpace(resp.Auth.Attributes["api_key"]) != "", len(resp.Auth.StorageJSON))
		return okEnvelope(resp), nil
	case pluginabi.MethodAuthLoginStart:
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth login request body"), nil
		}
		_, err := (authProvider{}).StartLogin(context.Background(), req.AuthLoginStartRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthLoginPoll:
		var req struct{ pluginapi.AuthLoginPollRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth poll request body"), nil
		}
		_, err := (authProvider{}).PollLogin(context.Background(), req.AuthLoginPollRequest)
		return ErrEnvelope("unsupported", err.Error()), nil
	case pluginabi.MethodAuthRefresh:
		var req struct{ pluginapi.AuthRefreshRequest }
		if json.Unmarshal(request, &req) != nil {
			return ErrEnvelope("invalid_request", "malformed auth refresh request body"), nil
		}
		resp, err := (authProvider{}).RefreshAuth(context.Background(), req.AuthRefreshRequest)
		if err != nil {
			return ErrEnvelope("auth_failure", err.Error()), nil
		}
		return okEnvelope(resp), nil
	case pluginabi.MethodSchedulerPick:
		return m.handleSchedulerPick(request)
	case pluginabi.MethodUsageHandle:
		return m.handleCodexUsage(request)
	case pluginabi.MethodRequestInterceptBefore:
		return m.handleInterceptBefore(request)
	case pluginabi.MethodRequestInterceptAfter:
		return m.handleInterceptAfter(request)
	case pluginabi.MethodRequestComplete:
		return m.handleRequestComplete(request)
	case pluginabi.MethodManagementRegister:
		return m.registerManagement(request)
	case pluginabi.MethodManagementHandle:
		return m.handleManagement(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": ProviderID}), nil
	case pluginabi.MethodExecutorCountTokens:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: "executor.count_tokens has no CommandCode equivalent",
		}), nil
	case pluginabi.MethodExecutorHTTPRequest:
		return classEnvelope(&errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("%s endpoint is not supported by commandcode", pluginabi.MethodExecutorHTTPRequest),
		}), nil
	default:
		return ErrEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func (m *Manager) handleGuardOnlySchedulerPick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed scheduler request body"), nil
	}
	available, blocked := m.codex.Filter(req.Candidates, time.Now())
	if !blocked {
		return okEnvelope(pluginapi.SchedulerPickResponse{}), nil
	}
	if len(available) == 0 {
		return okEnvelope(codexSchedulerReject()), nil
	}
	sort.SliceStable(available, func(i, j int) bool {
		return available[i].Priority > available[j].Priority
	})
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: available[0].ID}), nil
}

// lifecycleRequest mirrors rpcLifecycleRequest: config_yaml is base64.
// The request's schema_version is decoded-and-ignored; registration echoes
// pluginabi.SchemaVersion.
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type capabilities struct {
	ModelProvider             bool                         `json:"model_provider"`
	AuthProvider              bool                         `json:"auth_provider"`
	Executor                  bool                         `json:"executor"`
	ExecutorModelScope        pluginapi.ExecutorModelScope `json:"executor_model_scope,omitempty"`
	ExecutorInputFormats      []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats     []string                     `json:"executor_output_formats,omitempty"`
	ManagementAPI             bool                         `json:"management_api"`
	UsagePlugin               bool                         `json:"usage_plugin"`
	Scheduler                 bool                         `json:"scheduler"`
	SchedulerAcrossPriorities bool                         `json:"scheduler_across_priorities"`
	RequestInterceptor        bool                         `json:"request_interceptor"`
	RequestLifecyclePlugin    bool                         `json:"request_lifecycle_plugin"`
}

type registrationResult struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type managementRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

type managementResource struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description"`
}

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources"`
}

func registrationEnvelope(guardOnly bool) []byte {
	formats := []string{"openai", "claude", "openai-response"}
	caps := capabilities{
		ManagementAPI:             true,
		UsagePlugin:               true,
		Scheduler:                 true,
		SchedulerAcrossPriorities: true,
	}
	if !guardOnly {
		caps.ModelProvider = true
		caps.AuthProvider = true
		caps.Executor = true
		caps.ExecutorModelScope = pluginapi.ExecutorModelScopeOAuth
		caps.ExecutorInputFormats = formats
		caps.ExecutorOutputFormats = formats
		caps.RequestInterceptor = true
		caps.RequestLifecyclePlugin = true
	}
	return registrationResultEnvelope(caps)
}

func viewRegistrationEnvelope() []byte {
	return registrationResultEnvelope(capabilities{ManagementAPI: true})
}

func currentPluginVersion() string {
	if pluginRole == "view" {
		return viewPluginVersion
	}
	return pluginVersion
}

func registrationResultEnvelope(caps capabilities) []byte {
	return okEnvelope(registrationResult{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          currentPluginVersion(),
			Author:           pluginName,
			GitHubRepository: githubRepoURL,
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: caps,
	})
}

func (m *Manager) registerManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	routes := []managementRoute{
		{Method: "GET", Path: "/plugins/" + pluginName + "/accounts"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/accounts"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/accounts/import"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/accounts/delete"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/quota/refresh"},
		{Method: "GET", Path: "/plugins/" + pluginName + "/events"},
		{Method: "GET", Path: "/plugins/" + pluginName + "/status"},
		{Method: "GET", Path: "/plugins/" + pluginName + "/codex/bans"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/codex/unban"},
		{Method: "POST", Path: "/plugins/" + pluginName + "/codex/unban-all"},
		{Method: http.MethodPost, Path: "/plugins/" + pluginName + "/codex/bans/import"},
	}
	registeredResources := []managementResource{
		{Path: "/pool", Menu: "CommandCode 账号池", Description: "账号、真实额度、共享并发与调度记录。"},
		{Path: "/codex", Menu: "Codex 429 保护", Description: "窗口临时禁用、到期恢复与手动解禁。"},
	}
	if m.guardOnly() {
		routes = routes[7:]
		registeredResources = registeredResources[1:]
	}
	if pluginRole == "full" && (pluginName == nextPluginID || pluginName == updatePluginID) && !m.guardOnly() {
		for _, route := range routes {
			route.Path = strings.Replace(route.Path, "/plugins/"+pluginName, "/plugins/"+originalPluginID, 1)
			routes = append(routes, route)
		}
	}
	return okEnvelope(managementRegistration{Routes: routes, Resources: registeredResources}), nil
}

func (m *Manager) handleManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

func (m *Manager) registerViewManagement(request []byte) ([]byte, error) {
	var req struct {
		Plugin           pluginapi.Metadata `json:"Plugin"`
		BasePath         string             `json:"BasePath"`
		ResourceBasePath string             `json:"ResourceBasePath"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management registration request body"), nil
	}
	return okEnvelope(managementRegistration{
		Routes: []managementRoute{},
		Resources: []managementResource{
			{Path: "/pool", Menu: "CommandCode 账号池（兼容入口）", Description: "兼容入口：账号、额度与调度管理。"},
			{Path: "/codex", Menu: "Codex 429 保护（兼容入口）", Description: "兼容入口：窗口临时禁用与手动解禁。"},
		},
	}), nil
}

func (m *Manager) handleViewManagement(request []byte) ([]byte, error) {
	var req struct {
		pluginapi.ManagementRequest
		HostCallbackID string `json:"host_callback_id,omitempty"`
	}
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed management request body"), nil
	}
	resp, err := m.HandleManagement(context.Background(), req.ManagementRequest)
	if err != nil {
		return ErrEnvelope("management_failure", err.Error()), nil
	}
	return okEnvelope(resp), nil
}

func (m *Manager) handleViewLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return ErrEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		return ErrEnvelope("invalid_config", err.Error()), nil
	}
	if len(cfg.APIKeys) != 0 {
		return ErrEnvelope("invalid_config", "api-keys are not accepted in compatibility view mode"), nil
	}

	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.execClosing.Store(false)
	m.mu.Lock()
	m.hasValidRegistration = true
	m.poolActivationError = ""
	m.mu.Unlock()
	return viewRegistrationEnvelope(), nil
}

func (m *Manager) handleViewShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.execClosing.Store(true)
	m.mu.Lock()
	m.hasValidRegistration = false
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// handleLifecycle implements plugin.register / plugin.reconfigure: load
// config, refresh the catalog once, publish state, restart the refresh
// loop. Registration succeeds even when the initial refresh fails (FR-002).
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
		return m.lifecycleFailureEnvelope("invalid_request", "malformed lifecycle request body"), nil
	}
	cfg, err := config.Load(req.ConfigYAML)
	if err != nil {
		debugTrace("lifecycle config_error=%s", err.Error())
		m.lifeMu.Lock()
		defer m.lifeMu.Unlock()
		return m.lifecycleFailureEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.RLock()
	wasGuardOnly, previousConfig, previousManager := m.cfg.GuardOnly, m.cfg, m.mgr
	hadValidRegistration := m.hasValidRegistration
	m.mu.RUnlock()
	authDir := strings.TrimSpace(cfg.Pool.AuthDir)
	if authDir == "" {
		return m.lifecycleFailureEnvelope("invalid_config", "pool.auth-dir must match the CPA auth directory"), nil
	}
	authDir, err = filepath.Abs(authDir)
	if err != nil {
		return m.lifecycleFailureEnvelope("invalid_config", "pool auth directory is invalid"), nil
	}
	statePath := cfg.Pool.StatePath
	if statePath == "" {
		statePath = filepath.Join(authDir, ".commandcode-pool", "state.json")
	}
	statePath, err = filepath.Abs(statePath)
	if err != nil {
		return m.lifecycleFailureEnvelope("invalid_config", "pool state path is invalid"), nil
	}
	if m.pool != nil && cfg.GuardOnly {
		return m.lifecycleFailureEnvelope("invalid_config", "guard-only cannot be enabled while the account pool is active"), nil
	}
	if m.pool != nil && (authDir != m.poolAuthDir || statePath != m.poolStatePath) {
		return m.lifecycleFailureEnvelope("invalid_config", "pool storage paths cannot change while running"), nil
	}
	if m.pool != nil && m.cfg.Pool.QuotaMaxAge > 0 && (cfg.Pool.QuotaMaxAge != m.cfg.Pool.QuotaMaxAge || cfg.Pool.DefaultLimit != m.cfg.Pool.DefaultLimit) {
		return m.lifecycleFailureEnvelope("invalid_config", "pool default limit and quota max age require a restart"), nil
	}
	cfg.Pool.AuthDir, cfg.Pool.StatePath = authDir, statePath
	if !cfg.GuardOnly {
		if m.pool == nil {
			p, errNew := pool.New(pool.Config{
				AuthDir: authDir, StatePath: statePath, QuotaMaxAge: cfg.Pool.QuotaMaxAge,
				DefaultLimit: cfg.Pool.DefaultLimit, EventLimit: 500,
			})
			if errNew != nil {
				return m.lifecycleFailureEnvelope("invalid_config", "pool state could not be loaded"), nil
			}
			m.mu.Lock()
			m.pool, m.poolAuthDir, m.poolStatePath = p, authDir, statePath
			m.execClosing.Store(false)
			m.mu.Unlock()
		}
		if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
			if wasGuardOnly {
				m.mu.Lock()
				failedPool := m.pool
				m.pool, m.poolAuthDir, m.poolStatePath = nil, "", ""
				m.mu.Unlock()
				if failedPool != nil {
					failedPool.Close()
				}
			}
			return m.lifecycleFailureEnvelope("auth_materialization_failed", "configured accounts could not be imported"), nil
		}
	}
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	if m.poolQuotaStop != nil {
		m.poolQuotaStop()
		m.poolQuotaStop = nil
	}
	if cfg.GuardOnly {
		m.mu.Lock()
		m.cfg = cfg
		m.mgr = nil
		m.hasValidRegistration = true
		m.mu.Unlock()
		return registrationEnvelope(true), nil
	}
	cfg.APIKeys = m.catalogKeys()
	var client catalog.HostClient
	if m.bridge != nil {
		client = m.bridge
	}
	mgr := catalog.New(cfg, client)
	m.catalogMu.Lock()
	refreshErr := refreshOnce(context.Background(), mgr, m.bridge, registerRefreshTimeout, cfg)
	m.mu.Lock()
	m.cfg = cfg
	if refreshErr != nil && cfg.Catalog.StaleWhileUnavailable && m.mgr != nil && len(m.mgr.Models()) > 0 {
		mgr.SeedFrom(m.mgr)
	}
	m.mgr = mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	var publicationErr error
	if refreshErr == nil && len(cfg.APIKeys) > 0 {
		publicationErr = m.publishCatalogRevision(mgr)
	}
	m.catalogMu.Unlock()
	if publicationErr != nil {
		m.mu.Lock()
		m.stop, m.done = nil, nil
		m.mu.Unlock()
		if hadValidRegistration {
			m.mu.Lock()
			var failedPool *pool.Pool
			if wasGuardOnly {
				failedPool = m.pool
				m.pool, m.poolAuthDir, m.poolStatePath = nil, "", ""
			}
			m.cfg, m.mgr = previousConfig, previousManager
			var restartStop, restartDone chan struct{}
			if !wasGuardOnly && previousManager != nil {
				restartStop, restartDone = make(chan struct{}), make(chan struct{})
				m.stop, m.done = restartStop, restartDone
			}
			m.mu.Unlock()
			if failedPool != nil {
				failedPool.Close()
			}
			if !wasGuardOnly && previousManager != nil {
				m.startRefreshLoop(previousConfig, previousManager, previousConfig.Catalog.RefreshInterval, restartStop, restartDone)
				m.poolQuotaStop = m.startPoolQuotaLoop(previousConfig.Pool.QuotaRefreshInterval)
			}
			return m.lifecycleFailureEnvelope("catalog_publication_failed", "catalog publication failed"), nil
		}
		return ErrEnvelope("catalog_publication_failed", "catalog publication failed"), nil
	}
	m.startRefreshLoop(cfg, mgr, cfg.Catalog.RefreshInterval, stop, done)
	m.poolQuotaStop = m.startPoolQuotaLoop(cfg.Pool.QuotaRefreshInterval)
	m.mu.Lock()
	m.poolActivationError = ""
	m.hasValidRegistration = true
	m.mu.Unlock()
	return registrationEnvelope(false), nil
}

// authKeyHash is the non-secret digest that identifies one config key. It is
// independent of config ordering, which is what keeps the records derived from
// it idempotent across restarts.
func authKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// authRecordIDFromHash and authFileNameFromHash are the runtime auth ID and the
// auth file name for one key digest. Both derive from the digest alone so every
// producer agrees without recomputing it.
func authRecordIDFromHash(hash string) string { return ProviderID + "-key-" + hash + ".json" }
func authFileNameFromHash(hash string) string { return authRecordIDFromHash(hash) }

// 旧 api-keys 无真实主体信息，默认共享一个保守分组，不能据 Key 拆分上限。
func (m *Manager) materializeAuthRecords(_ context.Context, cfg config.Config) error {
	m.mu.RLock()
	p := m.pool
	m.mu.RUnlock()
	if p == nil {
		return fmt.Errorf("account pool unavailable")
	}
	for _, key := range cfg.APIKeys {
		if _, exists := p.Credential(authRecordIDFromHash(authKeyHash(key.Value))); exists {
			continue
		}
		if _, err := p.Upsert(pool.AccountInput{
			Name:    "Configured credential " + authKeyHash(key.Value)[:12],
			GroupID: "configured-accounts", APIKey: key.Value, MaxConcurrency: cfg.Pool.DefaultLimit,
		}); err != nil {
			return fmt.Errorf("configured account import failed")
		}
	}
	return nil
}

func (m *Manager) catalogKeys() []config.APIKey {
	m.mu.RLock()
	p := m.pool
	m.mu.RUnlock()
	keys := make([]config.APIKey, 0)
	if p != nil {
		for _, c := range p.Credentials() {
			if c.Enabled {
				keys = append(keys, config.APIKey{Value: c.APIKey})
			}
		}
	}
	return keys
}

func (m *Manager) refreshCatalogNow(ctx context.Context) error {
	m.catalogMu.Lock()
	defer m.catalogMu.Unlock()
	m.mu.RLock()
	cfg, mgr := m.cfg, m.mgr
	closing := m.execClosing.Load()
	m.mu.RUnlock()
	if closing || mgr == nil {
		return fmt.Errorf("catalog unavailable")
	}
	cfg.APIKeys = m.catalogKeys()
	if len(cfg.APIKeys) == 0 {
		return nil
	}
	if err := refreshOnce(ctx, mgr, m.bridge, registerRefreshTimeout, cfg); err != nil {
		return err
	}
	return m.publishCatalogRevision(mgr)
}

// 先安装可读目录，再重写 auth revision 穿过宿主的文件哈希与 metadata 去重两关。
// 调用方持 catalogMu；不得再取 lifeMu，重配置会持 lifeMu 等待旧 ticker 退出。
func (m *Manager) publishCatalogRevision(mgr *catalog.Manager) error {
	m.mu.RLock()
	p, current, closing := m.pool, m.mgr, m.execClosing.Load()
	m.mu.RUnlock()
	if closing || mgr == nil || current != mgr {
		return fmt.Errorf("catalog publication failed")
	}
	if p == nil || len(p.Snapshot()) == 0 {
		return nil
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("catalog publication failed")
	}
	if err := p.RepublishCatalog(hex.EncodeToString(nonce[:])); err != nil {
		return fmt.Errorf("catalog publication failed")
	}
	return nil
}

func (m *Manager) handleModelsForAuth(request []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if json.Unmarshal(request, &req) != nil {
		return ErrEnvelope("invalid_request", "malformed auth model request body"), nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if req.AuthProvider != ProviderID || m.execClosing.Load() || m.pool == nil {
		return emptyPoolModels(), nil
	}
	credential, known := m.pool.Credential(req.AuthID)
	if !known || !credential.Enabled || credential.APIKey == "" {
		return emptyPoolModels(), nil
	}
	return catalogModelsEnvelope(m.mgr), nil
}

func emptyPoolModels() []byte {
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: []pluginapi.ModelInfo{}})
}

// handleModels implements model.static: the last good catalog snapshot; auth
// discovery separately verifies the pool's current credential state.
func (m *Manager) handleModels() ([]byte, error) {
	m.mu.RLock()
	mgr := m.mgr
	m.mu.RUnlock()
	return catalogModelsEnvelope(mgr), nil
}

func catalogModelsEnvelope(mgr *catalog.Manager) []byte {
	models := make([]pluginapi.ModelInfo, 0)
	if mgr != nil {
		for _, rec := range mgr.Models() {
			models = append(models, pluginapi.ModelInfo{
				ID:                        rec.PublicID,
				Object:                    "model",
				OwnedBy:                   ProviderID,
				DisplayName:               rec.DisplayName,
				ContextLength:             rec.ContextLimit,
				MaxCompletionTokens:       rec.OutputLimit,
				SupportedInputModalities:  rec.InputModes,
				SupportedOutputModalities: rec.OutputModes,
				Thinking:                  rec.Thinking,
			})
		}
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: ProviderID, Models: models})
}

// handleShutdown stops the refresh loop, drains orphaned host callbacks,
// and clears state. The drain matters on Unix: the SDK loader frees host_api
// and dlclose's the plugin immediately after this export returns
// (loader_unix.go), so a goroutine still calling into the host would crash
// the process — see COMPATIBILITY.md limitations.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()
	m.mu.Lock()
	m.execClosing.Store(true)
	p := m.pool
	m.mu.Unlock()
	if p != nil {
		p.Close()
	}
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	if m.poolQuotaStop != nil {
		m.poolQuotaStop()
		m.poolQuotaStop = nil
	}
	m.executionWG.Wait()
	m.managementWG.Wait()
	// dlclose 后不得遗留仍调用宿主 C 指针的 goroutine，不能超时后继续卸载。
	if m.bridge != nil {
		m.bridge.inFlight.Wait()
	}
	m.mu.Lock()
	m.cfg, m.mgr, m.pool = config.Config{}, nil, nil
	m.poolAuthDir, m.poolStatePath = "", ""
	m.poolActivationError = ""
	m.hasValidRegistration = false
	m.mu.Unlock()
	return okEnvelope(struct{}{}), nil
}

// startRefreshLoop spawns the single background ticker goroutine over the
// caller-supplied stop/done pair (already stored under m.mu) and the served
// catalog manager. The loop captures its own cfg/mgr/bridge snapshot so
// it never contends on m.mu; reconfigure swaps state and restarts the loop.
func (m *Manager) startRefreshLoop(cfg config.Config, mgr *catalog.Manager, interval time.Duration, stop, done chan struct{}) {
	// F4: tick refresh contexts derive from stopCtx so close(stop) aborts
	// an in-flight Refresh immediately instead of leaving lifeMu held until
	// the old config's request-timeout expires. The watcher goroutine is
	// required because the loop body blocks inside refreshOnce while a tick
	// runs and cannot select on stop itself.
	stopCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	go func() {
		ticker := time.NewTicker(interval)
		defer close(done)
		defer ticker.Stop()
		defer cancel()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				func() {
					defer func() {
						// CGO bridge calls can panic; a tick must never
						// take the host process down.
						if r := recover(); r != nil && m.bridge != nil {
							_ = m.bridge.Log("error", "catalog refresh panicked", nil)
						}
					}()
					m.catalogMu.Lock()
					defer m.catalogMu.Unlock()
					current := cfg
					current.APIKeys = m.catalogKeys()
					if len(current.APIKeys) == 0 || refreshOnce(stopCtx, mgr, m.bridge, cfg.RequestTimeout, current) != nil {
						return
					}
					if m.publishCatalogRevision(mgr) != nil && m.bridge != nil {
						_ = m.bridge.Log("warn", "catalog publication failed", nil)
					}
				}()
			}
		}
	}()
}

// closeStop signals a running loop to exit and clears the stop/done pair.
// It returns the loop's done channel — nil when no loop was running — and
// the CALLER waits on it after this returns, never while holding m.mu (F4).
func (m *Manager) closeStop() chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stop == nil {
		return nil
	}
	done := m.done
	close(m.stop)
	m.stop, m.done = nil, nil
	return done
}

// refreshOnce runs one bounded catalog refresh. Catalog refresh is not a client
// request, so the fallback uses the first configured key only and has no client
// selection, rotation, cooldown, or retry state. parent bounds-and-cancels
// the attempt: the lifecycle path passes context.Background() plus
// registerRefreshTimeout; ticker ticks pass the loop's stop-derived context
// plus the full request-timeout, so close(stop) aborts an in-flight tick
// (F4). On failure it logs a warn via host.log — error text is a redacted
// category label from the catalog package, never key material (FR-011).
func refreshOnce(parent context.Context, mgr *catalog.Manager, bridge *HostBridge, timeout time.Duration, cfg config.Config) error {
	if len(cfg.APIKeys) == 0 {
		return nil
	}
	// Fallback: configured materialization input is catalog-only; CPA client
	// authentication remains owned by its AuthProvider and CPA.
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	err := mgr.Refresh(ctx, cfg.APIKeys[0].Value)
	if err != nil {
		if bridge != nil {
			_ = bridge.Log("warn", "catalog refresh failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	// FR-010: surface normalization diagnostics with the post-refresh log.
	warnings := mgr.Warnings()
	if bridge == nil {
		return nil
	}
	if uns := mgr.Unsupported(); len(uns) > 0 {
		parts := make([]string, len(uns))
		for i, u := range uns {
			parts[i] = fmt.Sprintf("%q: %s", u.UpstreamID, u.Reason)
		}
		fields := map[string]any{"models": strings.Join(parts, ", ")}
		if len(warnings) > 0 {
			fields["warnings"] = strings.Join(warnings, "; ")
		}
		_ = bridge.Log("warn", "unsupported models excluded from routable catalog", fields)
	} else if len(warnings) > 0 {
		_ = bridge.Log("warn", "catalog normalization warnings",
			map[string]any{"warnings": strings.Join(warnings, "; ")})
	}
	return nil
}

func okEnvelope(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func ErrEnvelope(code, message string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: message}})
	return out
}
