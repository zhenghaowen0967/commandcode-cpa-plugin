package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

func compatibilityConfigYAML(authDir string) string {
	return "pool:\n  auth-dir: " + filepath.ToSlash(authDir) + "\n  state-path: " + filepath.ToSlash(filepath.Join(authDir, ".commandcode-pool", "state.json")) + "\napi-keys: []\n"
}

func registerCompatibilityView(t *testing.T, m *Manager, yamlText string) registrationResult {
	t.Helper()
	resp := mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, yamlText))
	var registration registrationResult
	decodeResult(t, resp, &registration)
	return registration
}

func TestNextFullRegistersAndDispatchesOriginalManagementAliases(t *testing.T) {
	testFullRegistersAndDispatchesOriginalManagementAliases(t, nextPluginID)
}

func TestUpdateFullRegistersAndDispatchesOriginalManagementAliases(t *testing.T) {
	testFullRegistersAndDispatchesOriginalManagementAliases(t, updatePluginID)
}

func testFullRegistersAndDispatchesOriginalManagementAliases(t *testing.T, hostID string) {
	t.Helper()
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = hostID, "full"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	m := newPoolHandlerManager(t)
	addPoolHandlerAccount(t, m, "compat-alias-fixture-key", "Fixture", "compatibility")

	resp, err := m.registerManagement([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var registration managementRegistration
	decodeResult(t, resp, &registration)
	if len(registration.Routes) != 22 {
		t.Fatalf("registered route count = %d, want 11 next routes and 11 original aliases", len(registration.Routes))
	}
	var nextRoutes, originalAliases int
	for _, route := range registration.Routes {
		switch {
		case strings.HasPrefix(route.Path, "/plugins/"+hostID+"/"):
			nextRoutes++
		case strings.HasPrefix(route.Path, "/plugins/"+originalPluginID+"/"):
			originalAliases++
		default:
			t.Fatalf("unexpected management route: %+v", route)
		}
	}
	if nextRoutes != 11 || originalAliases != 11 {
		t.Fatalf("next routes=%d original aliases=%d", nextRoutes, originalAliases)
	}

	legacyPath := "/v0/management/plugins/" + originalPluginID + "/accounts"
	got, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: legacyPath})
	if err != nil || got.StatusCode != http.StatusOK || !strings.Contains(string(got.Body), "Fixture") {
		t.Fatalf("original management alias did not dispatch to full manager: response=%+v err=%v", got, err)
	}
	for _, page := range []string{"pool", "codex"} {
		resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodGet, Path: "/v0/resource/plugins/" + hostID + "/" + page,
		})
		if err != nil || resource.StatusCode != http.StatusOK || !strings.Contains(string(resource.Body), "/v0/management/plugins/"+hostID) {
			t.Fatalf("%s resource does not point to its own management namespace", hostID)
		}
	}
	resource, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + originalPluginID + "/pool",
	})
	if err != nil || resource.StatusCode != http.StatusNotFound {
		t.Fatalf("resource URL unexpectedly aliased across HostIDs: response=%+v err=%v", resource, err)
	}
}

func TestNextGuardOnlyDoesNotRegisterOrDispatchOriginalAliases(t *testing.T) {
	testGuardOnlyDoesNotRegisterOrDispatchOriginalAliases(t, nextPluginID)
}

func TestUpdateGuardOnlyDoesNotRegisterOrDispatchOriginalAliases(t *testing.T) {
	testGuardOnlyDoesNotRegisterOrDispatchOriginalAliases(t, updatePluginID)
}

func testGuardOnlyDoesNotRegisterOrDispatchOriginalAliases(t *testing.T, hostID string) {
	t.Helper()
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = hostID, "full"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	m := NewManager(nil)
	registerBridgeMode(t, m, guardOnlyConfigYAML(t.TempDir()))
	resp, err := m.registerManagement([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var registration managementRegistration
	decodeResult(t, resp, &registration)
	if len(registration.Routes) != 4 {
		t.Fatalf("guard-only route count = %d, want only four Codex routes", len(registration.Routes))
	}
	for _, route := range registration.Routes {
		if !strings.HasPrefix(route.Path, "/plugins/"+hostID+"/codex/") || strings.Contains(route.Path, "/plugins/"+originalPluginID+"/") {
			t.Fatalf("guard-only registered original alias or non-Codex route: %+v", route)
		}
	}
	got, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/management/plugins/" + hostID + "/codex/bans",
	})
	if err != nil || got.StatusCode != http.StatusOK || !strings.Contains(string(got.Body), `"guard_only":true`) {
		t.Fatalf("guard-only own namespace did not reach its own manager: response=%+v err=%v", got, err)
	}
	got, err = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/management/plugins/" + originalPluginID + "/codex/bans",
	})
	if err != nil || got.StatusCode != http.StatusNotFound {
		t.Fatalf("guard-only accepted an original-ID alias: response=%+v err=%v", got, err)
	}
}

func TestCompatibilityViewPublishesOnlyOriginalResourcesPointingToNextAPI(t *testing.T) {
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = originalPluginID, "view"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	m := NewManager(nil)
	registration := registerCompatibilityView(t, m, compatibilityConfigYAML(t.TempDir()))
	caps := registration.Capabilities
	if registration.Metadata.Name != originalPluginID || registration.Metadata.Version != viewPluginVersion ||
		!caps.ManagementAPI || caps.ModelProvider || caps.AuthProvider || caps.Executor || caps.UsagePlugin ||
		caps.Scheduler || caps.SchedulerAcrossPriorities || caps.RequestInterceptor || caps.RequestLifecyclePlugin || ProviderID != originalPluginID {
		t.Fatalf("view registration identity/capabilities = metadata:%+v caps:%+v provider:%q", registration.Metadata, caps, ProviderID)
	}
	if m.pool != nil || m.mgr != nil || len(m.cfg.APIKeys) != 0 || m.execClosing.Load() {
		t.Fatalf("view lifecycle initialized full-mode state: pool=%p catalog=%p api_keys=%d closing=%t", m.pool, m.mgr, len(m.cfg.APIKeys), m.execClosing.Load())
	}

	resp := mustHandle(t, m, pluginabi.MethodManagementRegister, []byte(`{}`))
	var management managementRegistration
	decodeResult(t, resp, &management)
	if len(management.Routes) != 0 || len(management.Resources) != 2 || management.Resources[0].Path != "/pool" || management.Resources[1].Path != "/codex" {
		t.Fatalf("view management registration = %+v", management)
	}
	for _, resource := range management.Resources {
		if !strings.Contains(resource.Menu, "兼容入口") {
			t.Fatalf("compatibility resource menu is not distinguished: %+v", resource)
		}
	}
	resourceRequest, err := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet, Path: "/v0/resource/plugins/" + originalPluginID + "/pool",
	})
	if err != nil {
		t.Fatal(err)
	}
	resourceResponse := mustHandle(t, m, pluginabi.MethodManagementHandle, resourceRequest)
	var dispatchedResource pluginapi.ManagementResponse
	decodeResult(t, resourceResponse, &dispatchedResource)
	if dispatchedResource.StatusCode != http.StatusOK || !strings.Contains(string(dispatchedResource.Body), "/v0/management/plugins/"+nextPluginID) {
		t.Fatalf("management.handle did not serve the compatibility resource: %+v", dispatchedResource)
	}
	for _, page := range []string{"pool", "codex"} {
		got, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodGet, Path: "/v0/resource/plugins/" + originalPluginID + "/" + page,
		})
		if err != nil || got.StatusCode != http.StatusOK {
			t.Fatalf("view resource %s failed: response=%+v err=%v", page, got, err)
		}
		body := string(got.Body)
		if !strings.Contains(body, "/v0/management/plugins/"+nextPluginID) || strings.Contains(body, "/v0/management/plugins/"+originalPluginID+"/") {
			t.Fatalf("view resource %s does not route API calls to next", page)
		}
	}
}

func TestCompatibilityViewDoesNotOpenHeldAuthDirectory(t *testing.T) {
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = originalPluginID, "view"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	authDir := t.TempDir()
	statePath := filepath.Join(authDir, ".commandcode-pool", "state.json")
	heldPool, err := pool.New(pool.Config{AuthDir: authDir, StatePath: statePath, QuotaMaxAge: time.Minute, DefaultLimit: 2, EventLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(nil)
	registerCompatibilityView(t, m, compatibilityConfigYAML(authDir))
	if m.pool != nil {
		t.Fatal("compatibility view opened an account pool")
	}
	if _, err := pool.New(pool.Config{AuthDir: authDir, StatePath: statePath, QuotaMaxAge: time.Minute, DefaultLimit: 2, EventLimit: 100}); err == nil {
		t.Fatal("registration unexpectedly released the independently held pool lock")
	}
	heldPool.Close()
	viewDidNotHoldLock, err := pool.New(pool.Config{AuthDir: authDir, StatePath: statePath, QuotaMaxAge: time.Minute, DefaultLimit: 2, EventLimit: 100})
	if err != nil {
		t.Fatalf("compatibility view retained a pool lock after owner closed: %v", err)
	}
	viewDidNotHoldLock.Close()
}

func TestCompatibilityViewRejectsExecutionAndAccountWrites(t *testing.T) {
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = originalPluginID, "view"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	m := NewManager(nil)
	registerCompatibilityView(t, m, compatibilityConfigYAML(t.TempDir()))
	for _, method := range []string{
		pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth,
		pluginabi.MethodAuthIdentifier, pluginabi.MethodAuthParse, pluginabi.MethodAuthLoginStart,
		pluginabi.MethodAuthLoginPoll, pluginabi.MethodAuthRefresh, pluginabi.MethodSchedulerPick,
		pluginabi.MethodUsageHandle, pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream,
		pluginabi.MethodExecutorIdentifier, pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest,
		pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter, pluginabi.MethodRequestComplete,
	} {
		resp := mustHandle(t, m, method, []byte(`{}`))
		if envelope := decodeEnv(t, resp); envelope.OK || envelope.Error == nil || envelope.Error.Code != "unsupported" {
			t.Fatalf("view method %s was not rejected as unsupported: %s", method, resp)
		}
	}
	for _, id := range []string{originalPluginID, nextPluginID} {
		got, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{
			Method: http.MethodPost, Path: "/v0/management/plugins/" + id + "/accounts", Body: []byte(`{"api_key":"must-not-be-written"}`),
		})
		if err != nil || got.StatusCode != http.StatusNotFound {
			t.Fatalf("view accepted account write via %s: response=%+v err=%v", id, got, err)
		}
	}
	if m.pool != nil || m.mgr != nil || m.UsageEventsSeen() != 0 || len(m.codex.Snapshot(time.Now())) != 0 {
		t.Fatalf("unsupported view calls changed runtime state: pool=%p catalog=%p usage=%d holds=%+v", m.pool, m.mgr, m.UsageEventsSeen(), m.codex.Snapshot(time.Now()))
	}

	const secret = "view-config-secret-must-not-persist"
	invalid := "api-keys:\n  - value: " + secret + "\n"
	resp := mustHandle(t, m, pluginabi.MethodPluginReconfigure, lifecycleRequestBody(t, invalid))
	envelope := decodeEnv(t, resp)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_config" || strings.Contains(string(resp), secret) || len(m.cfg.APIKeys) != 0 {
		t.Fatalf("view accepted or retained a legacy API key: %s cfg=%+v", resp, m.cfg)
	}
	if m.pool != nil || m.mgr != nil || m.UsageEventsSeen() != 0 {
		t.Fatalf("rejected view configuration initialized full-mode state: pool=%p catalog=%p usage=%d", m.pool, m.mgr, m.UsageEventsSeen())
	}
}

func TestCompatibilityViewShutdownDoesNotClearNextGuard(t *testing.T) {
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = originalPluginID, "view"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	view := NewManager(nil)
	registerCompatibilityView(t, view, compatibilityConfigYAML(t.TempDir()))
	next := NewManager(nil)
	next.codex.Record(pluginapi.UsageRecord{
		Provider: "codex", AuthID: "next-manager-held-codex", Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
	}, time.Now())
	before := next.codex.Snapshot(time.Now())
	mustHandle(t, view, pluginabi.MethodPluginShutdown, nil)
	if !view.execClosing.Load() || view.hasValidRegistration {
		t.Fatalf("view shutdown did not close its own registration: closing=%t registered=%t", view.execClosing.Load(), view.hasValidRegistration)
	}
	after := next.codex.Snapshot(time.Now())
	if len(before) != 1 || len(after) != 1 || after[0].AuthID != before[0].AuthID {
		t.Fatalf("view shutdown changed next manager guard: before=%+v after=%+v", before, after)
	}
	registerCompatibilityView(t, view, compatibilityConfigYAML(t.TempDir()))
	if view.execClosing.Load() {
		t.Fatal("successful compatibility re-registration left execClosing set")
	}
}

func TestCompatibilityViewRejectsMalformedLifecycleAndManagementRequests(t *testing.T) {
	originalName, originalRole := pluginName, pluginRole
	pluginName, pluginRole = originalPluginID, "view"
	defer func() { pluginName, pluginRole = originalName, originalRole }()

	m := NewManager(nil)
	for _, tc := range []struct {
		method string
		body   []byte
		code   string
	}{
		{method: pluginabi.MethodPluginRegister, body: []byte("{"), code: "invalid_request"},
		{method: pluginabi.MethodManagementRegister, body: []byte("{"), code: "invalid_request"},
		{method: pluginabi.MethodManagementHandle, body: []byte("{"), code: "invalid_request"},
	} {
		resp := mustHandle(t, m, tc.method, tc.body)
		envelope := decodeEnv(t, resp)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != tc.code {
			t.Fatalf("view method %s ignored malformed request: %s", tc.method, resp)
		}
	}
}
