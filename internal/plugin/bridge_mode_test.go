package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

func guardOnlyConfigYAML(authDir string) string {
	return "guard-only: true\npool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  state-path: " + filepath.ToSlash(filepath.Join(authDir, ".commandcode-pool", "state.json")) + "\napi-keys: []\n"
}

func activeBridgeConfigYAML(authDir string) string {
	return "pool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  state-path: " + filepath.ToSlash(filepath.Join(authDir, ".commandcode-pool", "state.json")) + "\napi-keys: []\n"
}

func registerBridgeMode(t *testing.T, m *Manager, yamlText string) registrationResult {
	t.Helper()
	resp := mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, yamlText))
	var registration registrationResult
	decodeResult(t, resp, &registration)
	return registration
}

func TestGuardOnlyDoesNotOpenPoolOrPublishPoolManagement(t *testing.T) {
	authDir := t.TempDir()
	m := NewManager(nil)
	registration := registerBridgeMode(t, m, guardOnlyConfigYAML(authDir))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })

	if registration.Metadata.Name != "commandcode-pool" || registration.Capabilities.ModelProvider ||
		registration.Capabilities.AuthProvider || registration.Capabilities.Executor ||
		!registration.Capabilities.UsagePlugin || !registration.Capabilities.Scheduler ||
		!registration.Capabilities.ManagementAPI || registration.Capabilities.RequestInterceptor ||
		registration.Capabilities.RequestLifecyclePlugin {
		t.Fatalf("guard-only registration exposes wrong capabilities: %+v", registration)
	}
	stateDir := filepath.Join(authDir, ".commandcode-pool")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("guard-only registration touched pool state directory: stat err=%v", err)
	}

	var mgmt managementRegistration
	resp, err := m.registerManagement([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, resp, &mgmt)
	if len(mgmt.Resources) != 1 || mgmt.Resources[0].Path != "/codex" || len(mgmt.Routes) != 4 {
		t.Fatalf("guard-only management registration exposes pool routes: %+v", mgmt)
	}
	for _, route := range mgmt.Routes {
		if strings.Contains(route.Path, "/accounts") || strings.Contains(route.Path, "/quota") {
			t.Fatalf("guard-only registered pool route: %+v", route)
		}
	}

	// guard-only 状态不得持有 Pool 锁。
	p, err := pool.New(pool.Config{
		AuthDir: authDir, StatePath: filepath.Join(stateDir, "state.json"),
		QuotaMaxAge: 5 * time.Minute, DefaultLimit: 2, EventLimit: 500,
	})
	if err != nil {
		t.Fatalf("guard-only manager unexpectedly held the pool lock: %v", err)
	}
	p.Close()

	poolPage, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/pool",
	})
	if err != nil || poolPage.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(poolPage.Body), "guard-only") {
		t.Fatalf("pool resource should clearly report unavailable: response=%+v err=%v", poolPage, err)
	}
}

func TestGuardOnlyModeSwitchKeepsGuardAndUsageWatermark(t *testing.T) {
	authDir := t.TempDir()
	m := NewManager(nil)
	registerBridgeMode(t, m, guardOnlyConfigYAML(authDir))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })

	holdCodex(t, m, "bridge-held-codex")
	if got := m.UsageEventsSeen(); got != 1 {
		t.Fatalf("usage watermark = %d, want first parsed event", got)
	}
	holdBefore := m.codex.Snapshot(time.Now())

	bad, _ := m.HandleCall(pluginabi.MethodUsageHandle, []byte("{"))
	if m.UsageEventsSeen() != 1 || !strings.Contains(string(bad), "invalid_request") {
		t.Fatalf("malformed usage advanced watermark: count=%d response=%s", m.UsageEventsSeen(), bad)
	}
	m.execClosing.Store(true)
	_, _ = m.HandleCall(pluginabi.MethodUsageHandle, poolHookBody(t, pluginapi.UsageRecord{Provider: "other-provider", AuthID: "other"}))
	m.execClosing.Store(false)
	if m.UsageEventsSeen() != 1 {
		t.Fatalf("closing usage advanced watermark: %d", m.UsageEventsSeen())
	}

	registerBridgeMode(t, m, activeBridgeConfigYAML(authDir))
	if m.pool == nil {
		t.Fatal("guard-only -> active transition did not open the existing pool path")
	}
	if got := m.UsageEventsSeen(); got != 1 {
		t.Fatalf("mode transition reset or advanced usage watermark: %d", got)
	}
	if got := m.codex.Snapshot(time.Now()); len(got) != len(holdBefore) || len(got) != 1 || got[0].AuthID != holdBefore[0].AuthID {
		t.Fatalf("mode transition replaced or cleared Codex Guard state: before=%+v after=%+v", holdBefore, got)
	}
}

func TestActivePoolReconfigureFailuresKeepLastValidRegistration(t *testing.T) {
	authDir := t.TempDir()
	m := NewManager(nil)
	registerBridgeMode(t, m, activeBridgeConfigYAML(authDir))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	activePool := m.pool
	assertActiveFallback := func(resp []byte) {
		t.Helper()
		var registration registrationResult
		decodeResult(t, resp, &registration)
		if !registration.Capabilities.ModelProvider || !registration.Capabilities.Executor ||
			!registration.Capabilities.Scheduler || m.pool != activePool || m.cfg.GuardOnly ||
			m.ActivationError() != poolActivationFailureMessage {
			t.Fatalf("failed reconfigure did not preserve full registration: caps=%+v pool_same=%t cfg=%+v activation_error=%q", registration.Capabilities, m.pool == activePool, m.cfg, m.ActivationError())
		}
	}

	resp, err := m.HandleCall(pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, guardOnlyConfigYAML(authDir)))
	if err != nil {
		t.Fatal(err)
	}
	assertActiveFallback(resp)

	resp, err = m.HandleCall(pluginabi.MethodPluginReconfigure, []byte("{"))
	if err != nil {
		t.Fatal(err)
	}
	assertActiveFallback(resp)

	invalidConfig := "pool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  max-concurrency: 0\napi-keys: []\n"
	resp = mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, invalidConfig))
	assertActiveFallback(resp)

	changedPoolConfig := "pool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  max-concurrency: 3\napi-keys: []\n"
	resp = mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, changedPoolConfig))
	assertActiveFallback(resp)

	registerBridgeMode(t, m, activeBridgeConfigYAML(authDir))
	if m.ActivationError() != "" || !m.hasValidRegistration || m.pool != activePool || m.cfg.GuardOnly {
		t.Fatalf("valid retry did not clear activation error and retain active state: error=%q pool_same=%t cfg=%+v", m.ActivationError(), m.pool == activePool, m.cfg)
	}
}

func TestFirstInvalidGuardOnlyRegistrationKeepsOriginalError(t *testing.T) {
	m := NewManager(nil)
	invalid := "guard-only: true\napi-keys:\n  - value: dummy-test-key\n"
	resp := mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, invalid))
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(resp, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_config" || envelope.Error.Message != "guard-only: api-keys must be empty" || m.hasValidRegistration {
		t.Fatalf("first invalid guard-only registration changed its error: %+v", envelope)
	}
}

func TestGuardOnlySchedulerPassesUnrelatedAndRejectsAllHeld(t *testing.T) {
	authDir := t.TempDir()
	m := NewManager(nil)
	registerBridgeMode(t, m, guardOnlyConfigYAML(authDir))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })

	unrelated := pluginapi.SchedulerAuthCandidate{ID: "non-codex", Provider: "other-provider", Priority: 1}
	if got := unifiedPick(t, m, unrelated); got.Handled || got.Reject || got.DelegateBuiltin != "" {
		t.Fatalf("unrelated scheduler request was claimed instead of builtin: %+v", got)
	}

	held := pluginapi.SchedulerAuthCandidate{ID: "codex-held", Provider: "codex", Priority: 20}
	holdCodex(t, m, held.ID)
	if got := unifiedPick(t, m, held); !got.Handled || !got.Reject || got.DelegateBuiltin != "" {
		t.Fatalf("all Codex candidates held fell through to builtin: %+v", got)
	}
}

func TestGuardOnlyActivationFailureRemainsProtectedAndCanRetry(t *testing.T) {
	authDir := t.TempDir()
	m := NewManager(nil)
	registerBridgeMode(t, m, guardOnlyConfigYAML(authDir))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	held := pluginapi.SchedulerAuthCandidate{ID: "activation-held", Provider: "codex", Priority: 10}
	holdCodex(t, m, held.ID)

	// 激活失败必须保留 guard-only 注册状态。
	blocker, err := pool.New(pool.Config{
		AuthDir: authDir, StatePath: filepath.Join(authDir, ".commandcode-pool", "state.json"),
		QuotaMaxAge: 5 * time.Minute, DefaultLimit: 2, EventLimit: 500,
	})
	if err != nil {
		t.Fatalf("open lock blocker: %v", err)
	}
	failed := mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, activeBridgeConfigYAML(authDir)))
	var stillGuard registrationResult
	decodeResult(t, failed, &stillGuard)
	if stillGuard.Capabilities.ModelProvider || stillGuard.Capabilities.Executor || !stillGuard.Capabilities.Scheduler ||
		m.ActivationError() != poolActivationFailureMessage || !m.cfg.GuardOnly || m.pool != nil {
		t.Fatalf("lock failure lost guard-only fallback: caps=%+v error=%q cfg=%+v pool=%p", stillGuard.Capabilities, m.ActivationError(), m.cfg, m.pool)
	}
	if m.UsageEventsSeen() != 1 || len(m.codex.Snapshot(time.Now())) != 1 {
		t.Fatalf("activation failure lost usage/hold: usage=%d bans=%+v", m.UsageEventsSeen(), m.codex.Snapshot(time.Now()))
	}
	if got := unifiedPick(t, m, held); !got.Handled || !got.Reject {
		t.Fatalf("held Codex candidate escaped after failed activation: %+v", got)
	}
	bans := callPoolManagement(t, m, http.MethodGet, "/codex/bans", "")
	var bridgeStatus struct {
		UsageEventsSeen uint64 `json:"usage_events_seen"`
		GuardOnly       bool   `json:"guard_only"`
		PoolActive      bool   `json:"pool_active"`
		ActivationError string `json:"activation_error"`
	}
	if bans.StatusCode != http.StatusOK || json.Unmarshal(bans.Body, &bridgeStatus) != nil ||
		bridgeStatus.UsageEventsSeen != 1 || !bridgeStatus.GuardOnly || bridgeStatus.PoolActive ||
		bridgeStatus.ActivationError != poolActivationFailureMessage {
		t.Fatalf("Codex status did not expose guard-only activation failure: %d %s", bans.StatusCode, bans.Body)
	}
	poolPage, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/pool",
	})
	if err != nil || poolPage.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("pool management was not unavailable after failed activation: %+v err=%v", poolPage, err)
	}
	blocker.Close()

	// 配置解析失败也必须保留当前有效注册。
	invalidConfig := "pool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  max-concurrency: 0\napi-keys: []\n"
	failed = mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, invalidConfig))
	decodeResult(t, failed, &stillGuard)
	if stillGuard.Capabilities.Executor || m.ActivationError() != poolActivationFailureMessage || !m.cfg.GuardOnly {
		t.Fatalf("invalid activation config discarded guard-only mode: caps=%+v error=%q", stillGuard.Capabilities, m.ActivationError())
	}

	activated := registerBridgeMode(t, m, activeBridgeConfigYAML(authDir))
	if !activated.Capabilities.ModelProvider || !activated.Capabilities.Executor || m.pool == nil || m.cfg.GuardOnly || m.ActivationError() != "" {
		t.Fatalf("valid retry did not activate pool and clear failure status: caps=%+v pool=%p guard_only=%t error=%q", activated.Capabilities, m.pool, m.cfg.GuardOnly, m.ActivationError())
	}
	if m.UsageEventsSeen() != 1 || len(m.codex.Snapshot(time.Now())) != 1 {
		t.Fatalf("successful retry reset usage/hold: usage=%d bans=%+v", m.UsageEventsSeen(), m.codex.Snapshot(time.Now()))
	}
}
func TestPluginNameDefaultAndLinkTimeManagementPrefix(t *testing.T) {
	if pluginName != "commandcode-pool" {
		t.Fatalf("default plugin name = %q, want commandcode-pool", pluginName)
	}
	original := pluginName
	pluginName = "commandcode-pool-next"
	t.Cleanup(func() { pluginName = original })

	var registration registrationResult
	decodeResult(t, registrationEnvelope(false), &registration)
	if registration.Metadata.Name != pluginName || ProviderID != "commandcode-pool" {
		t.Fatalf("plugin management name changed provider identity: name=%q provider=%q", registration.Metadata.Name, ProviderID)
	}

	m := NewManager(nil)
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/codex",
	})
	if err != nil || resource.StatusCode != http.StatusOK {
		t.Fatalf("Codex resource unavailable with linked name: response=%+v err=%v", resource, err)
	}
	page := string(resource.Body)
	if !strings.Contains(page, "/v0/management/plugins/commandcode-pool-next") ||
		strings.Contains(page, "/v0/management/plugins/commandcode-pool'") {
		t.Fatalf("resource management prefix was not switched to linked host ID")
	}
}
