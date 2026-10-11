package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"commandcode-cpa-plugin/internal/pool"
)

func TestPoolStatusReportsEffectiveStrategy(t *testing.T) {
	m := newPoolHandlerManager(t)
	addPoolHandlerAccount(t, m, "strategy-status-secret", "Account", "g")
	policy := func() string {
		t.Helper()
		resp := callPoolManagement(t, m, http.MethodGet, "/status", "")
		var got struct {
			Status struct {
				Policy string `json:"scheduler_policy"`
			} `json:"status"`
		}
		if json.Unmarshal(resp.Body, &got) != nil {
			t.Fatalf("status = %s", resp.Body)
		}
		return got.Status.Policy
	}
	if got := policy(); got != pool.StrategyHeadroomFirst {
		t.Fatalf("default scheduler_policy = %q, want %q", got, pool.StrategyHeadroomFirst)
	}
	if err := m.pool.SetStrategy(pool.StrategyExpiryFirst); err != nil {
		t.Fatal(err)
	}
	if got := policy(); got != pool.StrategyExpiryFirst {
		t.Fatalf("scheduler_policy after swap = %q, want %q", got, pool.StrategyExpiryFirst)
	}
}

func TestLifecycleStrategyConfiguredAndHotSwapped(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	yamlText := testValidYAML + "pool:\n  strategy: round-robin\n"
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, yamlText))); !env.OK {
		t.Fatalf("register rejected configured strategy: %+v", env.Error)
	}
	if got := m.pool.Strategy(); got != pool.StrategyRoundRobin {
		t.Fatalf("registered strategy = %q, want %q", got, pool.StrategyRoundRobin)
	}
	yamlText = testValidYAML + "pool:\n  strategy: expiry-first\n"
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, yamlText))); !env.OK {
		t.Fatalf("reconfigure rejected strategy swap: %+v", env.Error)
	}
	if got := m.pool.Strategy(); got != pool.StrategyExpiryFirst {
		t.Fatalf("hot-swapped strategy = %q, want %q", got, pool.StrategyExpiryFirst)
	}
}

func TestFailedReconfigureDoesNotLeakStrategyChange(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	activePool := m.pool
	// The strategy change travels with a config the pool cannot materialize (the
	// key exceeds the 4096-byte limit). The failed reconfigure falls back to the
	// last valid registration and must not leave the new strategy running.
	yamlText := "api-keys:\n  - value: " + strings.Repeat("k", 4097) + "\npool:\n  strategy: fill-first\n"
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, yamlText)), &registration)
	if m.pool != activePool || m.pool.Strategy() != pool.StrategyHeadroomFirst || m.ActivationError() != poolActivationFailureMessage {
		t.Fatalf("failed reconfigure leaked a strategy change: pool_same=%t strategy=%q error=%q", m.pool == activePool, m.pool.Strategy(), m.ActivationError())
	}
}

func TestRegisterRejectsUnknownStrategy(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML+"pool:\n  strategy: bogus\n")))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
		t.Fatalf("register with unknown strategy = %+v", env)
	}
	if m.pool != nil {
		t.Fatal("invalid config created a pool")
	}
}

func TestReconfigureRejectsUnknownStrategyAndKeepsCurrent(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall(pluginabi.MethodPluginShutdown, nil) })
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	activePool := m.pool
	// The invalid reconfigure must fall back to the last valid registration
	// instead of mutating the running pool.
	var registration registrationResult
	decodeResult(t, mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, testValidYAML+"pool:\n  strategy: bogus\n")), &registration)
	if m.pool != activePool || m.pool.Strategy() != pool.StrategyHeadroomFirst || m.ActivationError() != poolActivationFailureMessage {
		t.Fatalf("invalid strategy mutated the pool: pool_same=%t strategy=%q error=%q", m.pool == activePool, m.pool.Strategy(), m.ActivationError())
	}
}
