package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"

	"commandcode-cpa-plugin/internal/catalog"
	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/pool"
)

// Native background quota probes run even when catalog callbacks are mocked.
// Keep the whole plugin test process offline; a default endpoint fixture must
// fail locally instead of trying a real CommandCode endpoint with a fake key.
func TestMain(m *testing.M) {
	// This test-process-only bypass stops an environment proxy on loopback from
	// forwarding a disallowed remote target through an otherwise allowed dial.
	_ = os.Setenv("NO_PROXY", "*")
	_ = os.Setenv("no_proxy", "*")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	dial := (&net.Dialer{}).DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			return nil, errors.New("plugin test HTTP is restricted to loopback fixtures")
		}
		return dial(ctx, network, address)
	}
	http.DefaultTransport = transport
	code := m.Run()
	transport.CloseIdleConnections()
	os.Exit(code)
}

// ---- test doubles -------------------------------------------------------

const (
	testKey         = "sk-test-secret-1"
	testCatalogJSON = `{"data":[{"id":"glm-5.3"}]}`
	testValidYAML   = "api-keys:\n  - value: " + testKey + "\n"
	// testRouteOverrides pins the fixture models onto the messages and
	// responses routes: CommandCode serves OSS models on chat/completions
	// only, so every other upstream leg must be requested explicitly.
	testRouteOverrides = "route-overrides:\n" +
		"  minimax-m3:\n    protocol: messages\n    endpoint: /v1/messages\n" +
		"  gpt-5.6-luna:\n    protocol: responses\n    endpoint: /v1/responses\n"
	dummyKey     = "sk-test"
	dummyKeyYAML = "api-keys:\n  - value: " + dummyKey + "\n"
)

type capturedCall struct {
	method  string
	payload []byte
}

type fakeCaller struct {
	mu        sync.Mutex
	calls     []capturedCall
	responder func(method string, payload []byte) ([]byte, error)
	authFiles map[string]struct{}
}

func (f *fakeCaller) call(method string, payload []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, capturedCall{method: method, payload: payload})
	f.mu.Unlock()
	var raw []byte
	var err error
	if f.responder != nil {
		raw, err = f.responder(method, payload)
	} else {
		raw = hostOK(map[string]any{})
	}
	if method == pluginabi.MethodHostAuthList && err == nil && !hasExplicitAuthList(raw) {
		return f.authListResponse(), nil
	}
	if method == pluginabi.MethodHostAuthSave && err == nil && hostEnvelopeOK(raw) {
		var req pluginapi.HostAuthSaveRequest
		if json.Unmarshal(payload, &req) == nil && strings.TrimSpace(req.Name) != "" {
			f.mu.Lock()
			if f.authFiles == nil {
				f.authFiles = make(map[string]struct{})
			}
			f.authFiles[req.Name] = struct{}{}
			f.mu.Unlock()
		}
	}
	return raw, err
}

func hostEnvelopeOK(raw []byte) bool {
	var env pluginabi.Envelope
	return json.Unmarshal(raw, &env) == nil && env.OK
}

func hasExplicitAuthList(raw []byte) bool {
	var env pluginabi.Envelope
	if json.Unmarshal(raw, &env) != nil || !env.OK {
		return true
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(env.Result, &result) != nil {
		return true
	}
	_, ok := result["files"]
	return ok
}

func (f *fakeCaller) authListResponse() []byte {
	f.mu.Lock()
	files := make([]pluginapi.HostAuthFileEntry, 0, len(f.authFiles))
	for name := range f.authFiles {
		files = append(files, pluginapi.HostAuthFileEntry{
			ID: strings.TrimSuffix(name, ".json"), Name: name, Source: "file", Path: name,
		})
	}
	f.mu.Unlock()
	return hostOK(hostAuthListResponse{Files: files})
}

func (f *fakeCaller) recorded() []capturedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callsOf(method string) []capturedCall {
	var out []capturedCall
	for _, c := range f.recorded() {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func hostOK(result any) []byte {
	raw, _ := json.Marshal(result)
	out, _ := json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
	return out
}

func hostErr(code, msg string) []byte {
	out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: code, Message: msg}})
	return out
}

// catalogResponder answers host.http.do with a canned upstream response.
func catalogResponder(success bool, body string) func(string, []byte) ([]byte, error) {
	return func(method string, _ []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		if !success {
			return hostErr("upstream_down", "simulated upstream failure"), nil
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       []byte(body),
		}), nil
	}
}

func newTestManager(responder func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	f := &fakeCaller{responder: responder}
	return NewManager(NewHostBridge(f.call)), f
}

var lifecycleAuthDirs sync.Map // *testing.T -> one auth directory for every lifecycle in that test

// testPoolYAML injects only the storage location; explicit pool settings and
// every other configuration value are retained. Reconfigure/restart fixtures
// must use the same directory because storage paths cannot change at runtime.
func testPoolYAML(t *testing.T, yamlText string) string {
	t.Helper()
	var fields map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &fields); err != nil || fields == nil {
		return yamlText // Preserve malformed-config tests rather than repairing them.
	}
	poolFields, _ := fields["pool"].(map[string]any)
	if poolFields == nil {
		poolFields = make(map[string]any)
	}
	if dir, _ := poolFields["auth-dir"].(string); strings.TrimSpace(dir) == "" {
		dir, ok := lifecycleAuthDirs.Load(t)
		if !ok {
			dir, _ = lifecycleAuthDirs.LoadOrStore(t, t.TempDir())
			t.Cleanup(func() { lifecycleAuthDirs.Delete(t) })
		}
		poolFields["auth-dir"] = dir.(string)
	}
	fields["pool"] = poolFields
	raw, err := yaml.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal test lifecycle YAML: %v", err)
	}
	return string(raw)
}

func lifecycleRequestBody(t *testing.T, yamlText string) []byte {
	t.Helper()
	b, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(testPoolYAML(t, yamlText))})
	return b
}

func decodeEnv(t *testing.T, raw []byte) pluginabi.Envelope {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, raw)
	}
	return env
}

func decodeResult(t *testing.T, raw []byte, v any) {
	t.Helper()
	env := decodeEnv(t, raw)
	if !env.OK || env.Error != nil {
		t.Fatalf("expected OK envelope, got %s", raw)
	}
	if err := json.Unmarshal(env.Result, v); err != nil {
		t.Fatalf("decode result: %v (%s)", err, env.Result)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// installFastLoop rebuilds lifecycle state around a 5ms ticker. Validated
// configs floor catalog.refresh-interval at 1m, so tick-driven tests cannot
// obtain an observable interval through register/reconfigure; they install
// the production loop (startRefreshLoop) directly instead.
func installFastLoop(t *testing.T, m *Manager, yamlText string) {
	t.Helper()
	m.mu.RLock()
	needsRegister := m.pool == nil
	m.mu.RUnlock()
	if needsRegister {
		var reg registrationResult
		decodeResult(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, yamlText)), &reg)
	}
	cfg, err := config.Load([]byte(testPoolYAML(t, yamlText)))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	mgr := catalog.New(cfg, m.bridge)
	m.mu.Lock()
	m.cfg, m.mgr = cfg, mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, mgr, 5*time.Millisecond, stop, done)
}

// manualTick runs one tick body (refreshOnce) over the currently served
// state — exactly the work a background tick performs — without waiting out
// the 1m config floor on refresh-interval.
func manualTick(t *testing.T, m *Manager) {
	t.Helper()
	m.mu.RLock()
	mgr, cfg := m.mgr, m.cfg
	m.mu.RUnlock()
	if mgr == nil {
		t.Fatal("no served manager to tick")
	}
	if err := refreshOnce(context.Background(), mgr, m.bridge, time.Second, cfg); err != nil {
		t.Fatalf("tick refresh: %v", err)
	}
}

// ---- HostBridge ---------------------------------------------------------

func TestBridgeDoRoundTrip(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Errorf("method = %q, want host.http.do", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(pluginapi.HTTPResponse{
			StatusCode: http.StatusCreated,
			Headers:    http.Header{"X-Lane": []string{"fast"}},
			Body:       []byte("payload-bytes"),
		}), nil
	}}
	bridge := NewHostBridge(f.call)
	req := pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     "https://upstream.test/v1/models",
		Headers: http.Header{"Authorization": []string{"Bearer " + testKey}},
		Body:    []byte(`{"a":1}`),
	}
	resp, err := bridge.Do(context.Background(), req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.StatusCode != http.StatusCreated || string(resp.Body) != "payload-bytes" ||
		!reflect.DeepEqual(resp.Headers.Get("X-Lane"), "fast") {
		t.Fatalf("decoded response wrong: %+v", resp)
	}
	if got["method"] != http.MethodGet || got["url"] != "https://upstream.test/v1/models" {
		t.Fatalf("wire payload wrong: %v", got)
	}
	headers, _ := got["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+testKey {
		t.Fatalf("Authorization header missing: %v", headers)
	}
	if got["body"] != "eyJhIjoxfQ==" { // std base64 padding of {"a":1}
		t.Fatalf("body encoding wrong: %v", got["body"])
	}
}

func TestBridgeDoFailures(t *testing.T) {
	cases := []struct {
		name     string
		respond  func(string, []byte) ([]byte, error)
		wantSub  string
		zeroResp bool
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("boom") }, "boom", true},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("not-json"), nil }, "undecodable host response", true},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("denied", "nope"), nil }, "nope", true},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host http do failed:", true},
		{"malformed result", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true,"result":"{bad"}`), nil }, "undecodable response body", true},
		{"empty result decodes zero response", func(string, []byte) ([]byte, error) { return []byte(`{"ok":true}`), nil }, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bridge := NewHostBridge((&fakeCaller{responder: tc.respond}).call)
			resp, err := bridge.Do(context.Background(), pluginapi.HTTPRequest{})
			if tc.zeroResp {
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
				}
				if resp.StatusCode != 0 || resp.Body != nil {
					t.Fatalf("expected zero response, got %+v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestBridgeLogSuccess(t *testing.T) {
	var got map[string]any
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostLog {
			t.Errorf("method = %q, want host.log", method)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		return hostOK(map[string]any{}), nil
	}}
	err := NewHostBridge(f.call).Log("warn", "something happened", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if got["level"] != "warn" || got["message"] != "something happened" {
		t.Fatalf("log payload wrong: %v", got)
	}
	fields, _ := got["fields"].(map[string]any)
	if fields["k"] != "v" {
		t.Fatalf("fields wrong: %v", fields)
	}
}

func TestBridgeLogFailures(t *testing.T) {
	cases := []struct {
		name    string
		respond func(string, []byte) ([]byte, error)
		wantSub string
	}{
		{"caller error", func(string, []byte) ([]byte, error) { return nil, errors.New("log boom") }, "log boom"},
		{"undecodable envelope", func(string, []byte) ([]byte, error) { return []byte("{"), nil }, "undecodable host response"},
		{"envelope error with message", func(string, []byte) ([]byte, error) { return hostErr("busy", "later"), nil }, "later"},
		{"envelope error without object", func(string, []byte) ([]byte, error) { return []byte(`{"ok":false}`), nil }, "host log failed:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NewHostBridge((&fakeCaller{responder: tc.respond}).call).Log("warn", "m", nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestBridgeAuthSaveWireAndRedactsFailures(t *testing.T) {
	secret := "sk-auth-save-secret"
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthSave {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthSave)
		}
		var wire pluginapi.HostAuthSaveRequest
		if err := json.Unmarshal(payload, &wire); err != nil {
			t.Fatalf("payload not json: %v", err)
		}
		if wire.Name != "commandcode-key-test.json" || string(wire.JSON) != `{"type":"commandcode","api_key":"`+secret+`"}` {
			t.Fatalf("wire request = %+v", wire)
		}
		return hostOK(pluginapi.HostAuthSaveResponse{Name: wire.Name}), nil
	}}
	if err := NewHostBridge(f.call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{
		Name: "commandcode-key-test.json",
		JSON: json.RawMessage(`{"type":"commandcode","api_key":"` + secret + `"}`),
	}); err != nil {
		t.Fatalf("AuthSave: %v", err)
	}

	secretErr := "host rejected " + secret
	err := NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return hostErr("denied", secretErr), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "host auth save failed") {
		t.Fatalf("redacted auth error = %v", err)
	}
	err = NewHostBridge((&fakeCaller{responder: func(string, []byte) ([]byte, error) {
		return []byte("not-json"), nil
	}}).call).AuthSave(context.Background(), pluginapi.HostAuthSaveRequest{Name: "x.json", JSON: []byte(`{}`)})
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "undecodable host response") {
		t.Fatalf("malformed auth response = %v", err)
	}
}

func TestBridgeAuthListDecodesEntries(t *testing.T) {
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostAuthList {
			t.Fatalf("method = %q, want %q", method, pluginabi.MethodHostAuthList)
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			t.Fatalf("list payload not json: %v", err)
		}
		return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID: "commandcode-key-existing", Name: "commandcode-key-existing.json", Priority: 7,
		}}}), nil
	}}
	entries, err := NewHostBridge(f.call).AuthList(context.Background())
	if err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "commandcode-key-existing" || entries[0].Name != "commandcode-key-existing.json" || entries[0].Priority != 7 {
		t.Fatalf("auth entries = %+v", entries)
	}
}

// ---- dispatcher: registration ------------------------------------------

func TestRegisterSuccessPublishesModels(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != "commandcode-pool" || reg.Metadata.Version != pluginVersion ||
		len(reg.Metadata.ConfigFields) != 0 {
		t.Fatalf("metadata wrong: %+v", reg.Metadata)
	}
	if !reg.Capabilities.ModelProvider || !reg.Capabilities.AuthProvider || !reg.Capabilities.Executor ||
		!reg.Capabilities.Scheduler || !reg.Capabilities.SchedulerAcrossPriorities || !reg.Capabilities.UsagePlugin ||
		!reg.Capabilities.RequestInterceptor || !reg.Capabilities.RequestLifecyclePlugin {
		t.Fatalf("capabilities wrong: %+v", reg.Capabilities)
	}

	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	if wire["method"] != http.MethodGet {
		t.Fatalf("bridged method = %v, want GET", wire["method"])
	}
	if wire["url"] != "https://api.commandcode.ai/provider/v1/models" {
		t.Fatalf("bridged url = %v", wire["url"])
	}
	headers := wire["headers"].(map[string]any)
	auth := headers["Authorization"].([]any)[0].(string)
	if auth != "Bearer "+testKey {
		t.Fatalf("bridged Authorization = %q", auth)
	}

	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 1 {
		t.Fatalf("static = %+v", static)
	}
	got := static.Models[0]
	if got.ID != "commandcode/glm-5.3" || got.Object != "model" || got.OwnedBy != ProviderID ||
		got.DisplayName != "glm-5.3" || got.ContextLength != 0 || got.MaxCompletionTokens != 0 ||
		len(got.SupportedInputModalities) != 0 || len(got.SupportedOutputModalities) != 0 {
		t.Fatalf("model info = %+v", got)
	}

	var forAuth pluginapi.ModelResponse
	forAuthRequest, err := json.Marshal(pluginapi.AuthModelRequest{AuthID: testPoolAuthID(testKey), AuthProvider: ProviderID})
	if err != nil {
		t.Fatalf("marshal model.for_auth request: %v", err)
	}
	decodeResult(t, mustHandle(t, m, "model.for_auth", forAuthRequest), &forAuth)
	if forAuth.Provider != ProviderID || len(forAuth.Models) != 1 || forAuth.Models[0].ID != got.ID {
		t.Fatalf("for_auth = %+v", forAuth)
	}
}

func assertMaterializedPoolAuth(t *testing.T, m *Manager, keys ...string) {
	t.Helper()
	m.mu.RLock()
	p, authDir := m.pool, m.poolAuthDir
	m.mu.RUnlock()
	if p == nil {
		t.Fatal("lifecycle left no account pool")
	}
	files, err := filepath.Glob(filepath.Join(authDir, "commandcode-pool-key-*.json"))
	if err != nil || len(files) != len(keys) {
		t.Fatalf("auth files = %v err=%v, want %d", files, err, len(keys))
	}
	for _, view := range p.Snapshot() {
		if view.GroupID != "configured-accounts" || view.MaxConcurrency != 2 {
			t.Fatalf("configured accounts must retain the conservative shared cap of 2: id=%s group=%s cap=%d", view.ID, view.GroupID, view.MaxConcurrency)
		}
	}
	for _, key := range keys {
		hash := sha256.Sum256([]byte(key))
		id := "commandcode-pool-key-" + hex.EncodeToString(hash[:]) + ".json"
		credential, exists := p.Credential(id)
		if !exists || credential.AuthID != id || credential.APIKey != key ||
			!credential.Enabled || credential.GroupID != "configured-accounts" {
			t.Fatalf("configured credential identity/group/enabled state incorrect for %s", id)
		}
		raw, err := os.ReadFile(filepath.Join(authDir, id))
		if err != nil {
			t.Fatalf("read materialized auth %s: %v", id, err)
		}
		var record struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatalf("decode materialized auth %s: %v", id, err)
		}
		if record.Type != "commandcode-pool" || record.APIKey != key {
			t.Fatalf("materialized auth identity/key incorrect for %s", id)
		}
		if strings.Contains(id, key) || strings.Contains(credential.ID, key) || strings.Contains(credential.Name, key) {
			t.Fatalf("secret leaked in account identity for %s", id)
		}
		info, err := os.Stat(filepath.Join(authDir, id))
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("auth file %s must be private: info=%v err=%v", id, info, err)
		}
	}
	if info, err := os.Stat(filepath.Join(authDir, ".commandcode-pool", "state.json")); err != nil || info.IsDir() {
		t.Fatalf("default pool state not persisted in auth directory: info=%v err=%v", info, err)
	}
}

func TestLifecycleMaterializesDeterministicAuthRecords(t *testing.T) {
	first, second, third := "sk-materialize-a", "sk-materialize-b", "sk-materialize-c"
	yamlText := "api-keys:\n  - value: " + first + "\n  - value: " + second + "\n"
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, yamlText)), &reg)
	assertMaterializedPoolAuth(t, m, first, second)
	decodeResult(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(t, "api-keys:\n  - value: "+second+"\n  - value: "+first+"\n")), &reg)
	assertMaterializedPoolAuth(t, m, first, second)
	// YAML is a conservative bootstrap input, not the authority for deleting
	// accounts managed by the pool. Removing a key from YAML retains its record.
	decodeResult(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(t, "api-keys:\n  - value: "+first+"\n")), &reg)
	assertMaterializedPoolAuth(t, m, first, second)
	decodeResult(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(t, "api-keys:\n  - value: "+first+"\n  - value: "+third+"\n")), &reg)
	assertMaterializedPoolAuth(t, m, first, second, third)
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 0 {
		t.Fatalf("direct pool materialization must not call host.auth.save: %d calls", got)
	}
}

func TestLifecycleReloadsPoolStateAfterManagerRestart(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	first := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = first.HandleCall("plugin.shutdown", nil) })
	var reg registrationResult
	decodeResult(t, mustHandle(t, first, "plugin.register", lifecycleRequestBody(t, testValidYAML)), &reg)
	assertMaterializedPoolAuth(t, first, testKey)
	authDir := first.poolAuthDir
	credential, _ := first.pool.Credential(testPoolAuthID(testKey))
	// A mutable account field proves restart loaded pool state, not just the
	// same bootstrap key into a newly constructed identity.
	enabled := false
	if _, err := first.pool.Upsert(pool.AccountInput{ID: credential.ID, Name: "operator-renamed", GroupID: "operator-group", MaxConcurrency: 3, Enabled: &enabled}); err != nil {
		t.Fatalf("update persisted account: %v", err)
	}
	mustHandle(t, first, "plugin.shutdown", nil)

	second := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = second.HandleCall("plugin.shutdown", nil) })
	decodeResult(t, mustHandle(t, second, "plugin.register", lifecycleRequestBody(t, testValidYAML)), &reg)
	if second.poolAuthDir != authDir {
		t.Fatalf("restart auth directory = %q, want %q", second.poolAuthDir, authDir)
	}
	reloaded, exists := second.pool.Credential(testPoolAuthID(testKey))
	if !exists || reloaded.ID != credential.ID || reloaded.Name != "operator-renamed" || reloaded.GroupID != "operator-group" || reloaded.Enabled || reloaded.APIKey != testKey {
		t.Fatal("restart did not retain persisted operator edits and disabled state")
	}
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 0 {
		t.Fatalf("pool restart must not call host.auth.save: %d calls", got)
	}
}

func TestLifecycleDeletedAccountIsNotBootstrappedAgain(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, testValidYAML)), &reg)
	authDir := m.poolAuthDir
	credential, _ := m.pool.Credential(testPoolAuthID(testKey))
	if err := m.pool.Delete(credential.ID); err != nil {
		t.Fatalf("delete persisted account: %v", err)
	}
	assertDeleted := func(manager *Manager) {
		t.Helper()
		if _, err := os.Stat(filepath.Join(authDir, testPoolAuthID(testKey))); !os.IsNotExist(err) {
			t.Fatalf("deleted auth file was retained or recreated: %v", err)
		}
		if deleted, exists := manager.pool.Credential(testPoolAuthID(testKey)); exists && (deleted.Enabled || deleted.APIKey != "") {
			t.Fatal("deleted account was reactivated by unchanged bootstrap configuration")
		}
	}
	decodeResult(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(t, testValidYAML)), &reg)
	assertDeleted(m)
	mustHandle(t, m, "plugin.shutdown", nil)
	restarted := NewManager(m.bridge)
	t.Cleanup(func() { _, _ = restarted.HandleCall("plugin.shutdown", nil) })
	decodeResult(t, mustHandle(t, restarted, "plugin.register", lifecycleRequestBody(t, testValidYAML)), &reg)
	assertDeleted(restarted)
}

func TestLifecycleInvalidPoolStateDoesNotWriteAuth(t *testing.T) {
	authDir := t.TempDir()
	stateDir := filepath.Join(authDir, ".commandcode-pool")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := testValidYAML + fmt.Sprintf("pool:\n  auth-dir: %q\n", authDir)
	env := decodeEnv(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, yamlText)))
	if env.OK || env.Error == nil || env.Error.Code != "invalid_config" {
		t.Fatalf("invalid pool state envelope = %+v", env.Error)
	}
	files, err := filepath.Glob(filepath.Join(authDir, "*.json"))
	if err != nil || len(files) != 0 {
		t.Fatalf("invalid state materialized auth files = %v err=%v", files, err)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatal("invalid pool state must fail before auth/catalog callbacks")
	}
}

// TestRegisterMetadataCarriesGitHubRepository pins the host validity gate
// (pinned SDK host.go validPlugin): an empty GitHubRepository makes the real
// host drop the plugin on every register/reconfigure, so the field must be
// non-empty AND actually marshal into the envelope bytes.
func TestRegisterMetadataCarriesGitHubRepository(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var reg registrationResult
	decodeResult(t, resp, &reg)
	if reg.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata.GitHubRepository empty: host validPlugin would drop the plugin: %+v", reg.Metadata)
	}
	// Metadata structs have no json tags; assert the Go field name marshals.
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		t.Fatalf("envelope unmarshal: %v", err)
	}
	if !bytes.Contains(env.Result, []byte(`"GitHubRepository":`)) ||
		bytes.Contains(env.Result, []byte(`"GitHubRepository":""`)) {
		t.Fatalf("envelope metadata lacks non-empty GitHubRepository: %s", env.Result)
	}
}

func TestRegisterIgnoresInjectedHostKeys(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	yamlText := testValidYAML + "enabled: true\npriority: 42\n"
	resp, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(t, yamlText))
	if err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("host-injected keys rejected: %s", resp)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "commandcode/glm-5.3" {
		t.Fatalf("models = %+v", static.Models)
	}
}

func TestMalformedLifecycleAndUnknownMethods(t *testing.T) {
	m, _ := newTestManager(nil)
	for name, body := range map[string][]byte{
		"truncated json": []byte("{"),
		"bad base64":     []byte(`{"config_yaml":"!!!not-base64!!!","schema_version":3}`),
	} {
		resp, err := m.HandleCall("plugin.register", body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := decodeEnv(t, resp)
		if env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
			t.Fatalf("%s: envelope = %s", name, resp)
		}
	}
	resp, err := m.HandleCall("totally.bogus", nil)
	if err != nil {
		t.Fatalf("unknown method: %v", err)
	}
	env := decodeEnv(t, resp)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" ||
		!strings.Contains(env.Error.Message, "totally.bogus") {
		t.Fatalf("envelope = %s", resp)
	}
}

func TestReconfigureSwapsPrefixIDs(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	noPrefix := testValidYAML + "model-prefix:\n  enabled: false\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(t, noPrefix)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 || static.Models[0].ID != "glm-5.3" {
		t.Fatalf("after prefix-off reconfigure: %+v", static.Models)
	}
}

func TestRefreshFailureStillRegistersAndWarnsWithoutSecrets(t *testing.T) {
	m, f := newTestManager(catalogResponder(false, ""))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("refresh failure must not fail registration (FR-002): %s", resp)
	}
	logCalls := f.callsOf(pluginabi.MethodHostLog)
	if len(logCalls) != 1 {
		t.Fatalf("warn logs = %d, want 1", len(logCalls))
	}
	var entry map[string]any
	if err := json.Unmarshal(logCalls[0].payload, &entry); err != nil {
		t.Fatalf("log payload: %v", err)
	}
	if entry["level"] != "warn" || !strings.Contains(entry["message"].(string), "catalog refresh failed") {
		t.Fatalf("log entry = %v", entry)
	}
	blob := string(logCalls[0].payload)
	if strings.Contains(blob, testKey) || strings.Contains(blob, "Bearer") {
		t.Fatalf("log leaks credential material: %s", blob)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("failed refresh must yield empty routable set: %+v", static.Models)
	}
}

func TestRegisterRefreshesWithConfiguredBearer(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, dummyKeyYAML))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("register rejected: %s", resp)
	}
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	if len(calls) != 1 {
		t.Fatalf("host.http.do calls = %d, want 1", len(calls))
	}
	var wire map[string]any
	if err := json.Unmarshal(calls[0].payload, &wire); err != nil {
		t.Fatalf("bridge payload: %v", err)
	}
	headers := wire["headers"].(map[string]any)
	if headers["Authorization"].([]any)[0].(string) != "Bearer "+dummyKey {
		t.Fatalf("expected configured bearer key, got %v", headers)
	}
}

// TestRegisterWithBlockingCatalogReturnsQuickly pins the dedicated register
// timeout: the synchronous lifecycle refresh must expire at
// registerRefreshTimeout (10s), never at request-timeout (default 15m), and
// registration must still succeed with an empty routable set (FR-002).
func TestRegisterWithBlockingCatalogReturnsQuickly(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		var wire map[string]any
		if method == pluginabi.MethodHostHTTPDo && json.Unmarshal(payload, &wire) == nil {
			if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
				<-release // block far past the register timeout
				return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
			}
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() {
		releaseCallback()
		_, _ = m.HandleCall("plugin.shutdown", nil)
	})

	start := time.Now()
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("registration must succeed despite blocked catalog: %s", resp)
	}
	if elapsed < 9*time.Second || elapsed > 13*time.Second {
		t.Fatalf("register elapsed = %v, want ~registerRefreshTimeout (10s), not 15m", elapsed)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 0 {
		t.Fatalf("blocked refresh must yield empty routable set: %+v", static.Models)
	}
}

// ---- dispatcher: models before register, shutdown, panic ---------------

func TestModelsBeforeRegisterIsEmptyNotError(t *testing.T) {
	m, _ := newTestManager(nil)
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if static.Provider != ProviderID || len(static.Models) != 0 {
		t.Fatalf("pre-register static = %+v", static)
	}
}

func TestShutdownClearsStateAndStopsTicker(t *testing.T) {
	f := &fakeCaller{responder: catalogResponder(true, testCatalogJSON)}
	m := NewManager(NewHostBridge(f.call))
	installFastLoop(t, m, testValidYAML)
	waitFor(t, "periodic refresh ticks", func() bool {
		return len(f.callsOf(pluginabi.MethodHostHTTPDo)) >= 3
	})
	resp := mustHandle(t, m, "plugin.shutdown", nil)
	env := decodeEnv(t, resp)
	if !env.OK || string(env.Result) != "{}" {
		t.Fatalf("shutdown envelope = %s", resp)
	}
	m.mu.Lock()
	stopped := m.stop == nil && m.done == nil && m.mgr == nil && m.cfg.BaseURL == ""
	m.mu.Unlock()
	if !stopped {
		t.Fatal("shutdown left state behind")
	}
	before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	time.Sleep(40 * time.Millisecond)
	if after := len(f.callsOf(pluginabi.MethodHostHTTPDo)); after != before {
		t.Fatalf("ticks continued after shutdown: %d -> %d", before, after)
	}
	resp2 := mustHandle(t, m, "plugin.shutdown", nil)
	if env := decodeEnv(t, resp2); !env.OK {
		t.Fatalf("double shutdown must be idempotent: %s", resp2)
	}
}

// TestShutdownDrainsOrphanedHostCallbacks pins unload safety: a timed-out
// caller is not evidence that the raw callback stopped using host pointers.
// Shutdown must stay blocked until that callback really returns.
func TestShutdownDrainsOrphanedHostCallbacks(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo {
			<-release
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() {
		releaseCallback()
		_, _ = m.HandleCall("plugin.shutdown", nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected wedged Do to time out, got %v", err)
	}
	shutdown := make(chan []byte, 1)
	go func() {
		resp, _ := m.HandleCall("plugin.shutdown", nil)
		shutdown <- resp
	}()
	waitFor(t, "shutdown admission seal", m.execClosing.Load)
	select {
	case resp := <-shutdown:
		t.Fatalf("shutdown returned while a host callback still used host pointers: %s", resp)
	case <-time.After(100 * time.Millisecond):
	}
	releaseCallback()
	select {
	case resp := <-shutdown:
		if env := decodeEnv(t, resp); !env.OK || string(env.Result) != "{}" {
			t.Fatalf("shutdown envelope = %s", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after the host callback returned")
	}
	if env := decodeEnv(t, mustHandle(t, m, "plugin.shutdown", nil)); !env.OK {
		t.Fatal("repeated shutdown was not idempotent")
	}
}

// TestRegisterSurvivesNilHostBridge pins F5 degradation: a zero-value
// Manager (nil host bridge, only reachable via direct construction) makes
// the initial refresh fail with the catalog package's classified
// "host client unavailable" error instead of panicking; registration still
// succeeds under FR-002 stale semantics.
func TestRegisterSurvivesNilHostBridge(t *testing.T) {
	m := &Manager{}
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	resp, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML))
	if err != nil {
		t.Fatalf("register must not surface as Go error: %v", err)
	}
	env := decodeEnv(t, resp)
	if !env.OK {
		t.Fatalf("registration must survive unavailable host client: %s", resp)
	}
	var reg registrationResult
	if err := json.Unmarshal(env.Result, &reg); err != nil || reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("degraded registration malformed: %s", resp)
	}
}

// ---- concurrency --------------------------------------------------------

func TestConcurrentHandleCalls(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	methods := []string{"model.static", "model.for_auth", "bogus.method"}
	forAuthRequest, err := json.Marshal(pluginapi.AuthModelRequest{AuthID: testPoolAuthID(testKey), AuthProvider: ProviderID})
	if err != nil {
		t.Fatalf("marshal model.for_auth request: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(len(methods) + 1)
	for _, method := range methods {
		go func(method string) {
			defer wg.Done()
			var request []byte
			if method == "model.for_auth" {
				request = forAuthRequest
			}
			for i := 0; i < 25; i++ {
				if _, err := m.HandleCall(method, request); err != nil {
					t.Errorf("%s: %v", method, err)
					return
				}
			}
		}(method)
	}
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			body := lifecycleRequestBody(t, testValidYAML)
			if i%2 == 1 {
				body = lifecycleRequestBody(t, testValidYAML+"model-prefix:\n  enabled: false\n")
			}
			if _, err := m.HandleCall("plugin.reconfigure", body); err != nil {
				t.Errorf("reconfigure: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

func TestOverlappingLifecyclesLeaveSingleTicker(t *testing.T) {
	// F2/F4 regression: concurrent register/reconfigure must serialize the
	// stop-wait-install sequence so exactly one ticker survives, and it must
	// exit on stop (no orphaned loops keep refreshing). Validated configs
	// floor refresh-interval at 1m, so the survivor is checked structurally
	// — one tracked loop that exits on stop — instead of by counting ticks.
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			method := "plugin.register"
			if i == 1 {
				method = "plugin.reconfigure"
			}
			if _, err := m.HandleCall(method, lifecycleRequestBody(t, testValidYAML)); err != nil {
				t.Errorf("%s: %v", method, err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(f.callsOf(pluginabi.MethodHostAuthSave)); got != 0 {
		t.Fatalf("overlapping lifecycles used forbidden host.auth.save: %d calls", got)
	}
	assertMaterializedPoolAuth(t, m, testKey)
	done := m.closeStop()
	if done == nil {
		t.Fatal("concurrent lifecycles left no tracked ticker")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("surviving ticker did not exit on stop")
	}
	if again := m.closeStop(); again != nil {
		t.Fatal("more than one ticker was left tracked")
	}
}

func TestReconfigureFailedRefreshServesViaTicks(t *testing.T) {
	// F3 regression: when a reconfigure's synchronous refresh fails, the
	// stale-check keeps the SERVED manager; background ticks must refresh
	// that same manager so recovered upstream data becomes visible.
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var fetches atomic.Int64
	f.responder = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch fetches.Add(1) {
		case 1:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case 2:
			return hostErr("upstream_down", "simulated refresh failure"), nil
		default:
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		}
	}

	// Validated configs floor refresh-interval at 1m, so ticks are driven
	// manually via manualTick (the exact body a background tick runs).
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(t, testValidYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")
	manualTick(t, m)
	assertStaticModel(t, m, "commandcode/minimax-m3")
}

// TestShutdownAbortsInFlightTickRefresh distinguishes loop cancellation from
// raw callback completion: stop must unwind the refresh loop promptly, but
// unload cannot finish until the parked callback returns.
func TestShutdownAbortsInFlightTickRefresh(t *testing.T) {
	var syncServed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseCallback := func() { releaseOnce.Do(func() { close(release) }) }
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		if err := json.Unmarshal(payload, &wire); err != nil {
			return hostOK(map[string]any{}), nil
		}
		if url, _ := wire["url"].(string); strings.HasSuffix(url, "/models") {
			if !syncServed.CompareAndSwap(false, true) {
				close(started)
				<-release
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(testCatalogJSON)}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() {
		releaseCallback()
		_, _ = m.HandleCall("plugin.shutdown", nil)
	})
	var reg registrationResult
	decodeResult(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(t, testValidYAML)), &reg)
	installFastLoop(t, m, testValidYAML)
	m.mu.RLock()
	tickDone := m.done
	m.mu.RUnlock()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("tick refresh never started")
	}
	shutdown := make(chan []byte, 1)
	go func() {
		resp, _ := m.HandleCall("plugin.shutdown", nil)
		shutdown <- resp
	}()
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop-derived context did not unwind the tick refresh")
	}
	select {
	case resp := <-shutdown:
		t.Fatalf("shutdown returned before the raw tick callback finished: %s", resp)
	case <-time.After(100 * time.Millisecond):
	}
	releaseCallback()
	select {
	case resp := <-shutdown:
		if env := decodeEnv(t, resp); !env.OK {
			t.Fatalf("shutdown envelope = %s", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish once the tick callback returned")
	}
}

// TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL pins the F5
// fix: a failed-refresh reconfigure must adopt the NEW manager (seeded with
// the old last-good records) so carried-over models stay visible between the
// failure and the next success, and background ticks fetch the NEW
// catalog-url instead of the old one forever.
func TestReconfigureFailedRefreshSeedsCarryoverThenFetchesNewURL(t *testing.T) {
	oldURL := "https://api.commandcode.ai/provider/v1/models"
	newURL := "https://mirror.test/api/models"
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	var allowOldURL atomic.Bool
	allowOldURL.Store(true)
	var failNewSync atomic.Bool
	failNewSync.Store(true)
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire map[string]any
		_ = json.Unmarshal(payload, &wire)
		url, _ := wire["url"].(string)
		if !strings.HasSuffix(url, "/models") {
			return hostOK(map[string]any{}), nil
		}
		switch url {
		case oldURL:
			// Every /models fetch after the reconfigure returned must target
			// the NEW url; old-loop ticks all complete before it returns.
			if !allowOldURL.Load() {
				t.Error("tick used the OLD catalog-url after reconfigure")
				return hostErr("old_url", url), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
		case newURL:
			if failNewSync.CompareAndSwap(true, false) {
				return hostErr("upstream_down", "simulated reconfigure refresh failure"), nil
			}
			return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
		default:
			t.Errorf("unexpected catalog url %q", url)
			return hostErr("unexpected_url", url), nil
		}
	}}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	assertStaticModel(t, m, "commandcode/glm-5.3")

	newBaseYAML := testValidYAML + "base-url: https://mirror.test/api\n"
	if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(t, newBaseYAML)); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	allowOldURL.Store(false)

	// Carried-over last-good model stays visible between the failed
	// reconfigure refresh and the next successful refresh.
	assertStaticModel(t, m, "commandcode/glm-5.3")

	// Validated configs floor refresh-interval at 1m; run the tick body
	// directly over the served state instead of waiting for the loop.
	manualTick(t, m)
	assertStaticModel(t, m, "commandcode/minimax-m3")
}

// TestReconfigureFailedRefreshHonorsStalePolicy pins the seed-gate fix: on a
// failed-refresh reconfigure the seed consults the NEW config's
// stale-while-unavailable — fail-closed serves nothing immediately (matching
// Manager.fail's ticker behavior), stale-enabled keeps carrying over.
func TestReconfigureFailedRefreshHonorsStalePolicy(t *testing.T) {
	catalogV1 := `{"data":[{"id":"glm-5.3"}]}`
	catalogV2 := `{"data":[{"id":"minimax-m3"}]}`
	for _, tc := range []struct {
		name          string
		policyYAML    string
		wantImmediate bool // model visible right after the failed reconfigure
	}{
		{name: "stale enabled keeps carryover", policyYAML: "", wantImmediate: true},
		{name: "fail closed empties immediately", policyYAML: "catalog:\n  stale-while-unavailable: false\n", wantImmediate: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fetches atomic.Int64
			f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
				if method != pluginabi.MethodHostHTTPDo {
					return hostOK(map[string]any{}), nil
				}
				var wire map[string]any
				_ = json.Unmarshal(payload, &wire)
				if url, _ := wire["url"].(string); !strings.HasSuffix(url, "/models") {
					return hostOK(map[string]any{}), nil
				}
				switch fetches.Add(1) {
				case 1:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV1)}), nil
				case 2:
					return hostErr("upstream_down", "simulated refresh failure"), nil
				default:
					return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusOK, Body: []byte(catalogV2)}), nil
				}
			}}
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })

			if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(t, testValidYAML)); err != nil {
				t.Fatalf("register: %v", err)
			}
			assertStaticModel(t, m, "commandcode/glm-5.3")

			if _, err := m.HandleCall("plugin.reconfigure", lifecycleRequestBody(t, testValidYAML+tc.policyYAML)); err != nil {
				t.Fatalf("reconfigure: %v", err)
			}

			var static pluginapi.ModelResponse
			decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
			got := len(static.Models)
			if tc.wantImmediate && (got != 1 || static.Models[0].ID != "commandcode/glm-5.3") {
				t.Fatalf("carryover models = %+v, want glm-5.3", static.Models)
			}
			if !tc.wantImmediate && got != 0 {
				t.Fatalf("fail-closed served %d models immediately, want 0", got)
			}

			// Both policies recover via the next successful tick against the
			// NEW manager/config.
			manualTick(t, m)
			assertStaticModel(t, m, "commandcode/minimax-m3")
		})
	}
}

func staticModelID(t *testing.T, m *Manager) string {
	t.Helper()
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, "model.static", nil), &static)
	if len(static.Models) != 1 {
		t.Fatalf("static models = %+v", static.Models)
	}
	return static.Models[0].ID
}

func assertStaticModel(t *testing.T, m *Manager, wantID string) {
	t.Helper()
	if got := staticModelID(t, m); got != wantID {
		t.Fatalf("static model = %q, want %q", got, wantID)
	}
}

func mustHandle(t *testing.T, m *Manager, method string, req []byte) []byte {
	t.Helper()
	resp, err := m.HandleCall(method, req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return resp
}
