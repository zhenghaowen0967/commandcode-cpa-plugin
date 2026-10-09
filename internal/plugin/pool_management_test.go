package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/config"
	"commandcode-cpa-plugin/internal/pool"
	"commandcode-cpa-plugin/resources"
)

func newPoolHandlerManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	p, err := pool.New(pool.Config{AuthDir: dir, StatePath: filepath.Join(dir, ".pool", "state.json"), QuotaMaxAge: time.Minute, DefaultLimit: 2, EventLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	m := NewManager(nil)
	m.pool = p
	m.cfg = config.Config{BaseURL: "https://example.test/provider/v1", Pool: config.PoolConfig{
		AuthDir: dir, StatePath: filepath.Join(dir, ".pool", "state.json"), DefaultLimit: 2,
		QuotaMaxAge: time.Minute, QuotaRefreshInterval: 10 * time.Second,
	}}
	return m
}

func callPoolManagement(t *testing.T, m *Manager, method, endpoint, body string) pluginapi.ManagementResponse {
	t.Helper()
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: method, Path: "/v0/management/plugins/" + pluginName + endpoint, Body: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func addPoolHandlerAccount(t *testing.T, m *Manager, key, name, group string) pool.AccountView {
	t.Helper()
	view, err := m.pool.Upsert(pool.AccountInput{APIKey: key, Name: name, GroupID: group, MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestPoolManagementResourceAndLegacyNotExposed(t *testing.T) {
	m := NewManager(nil)
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/" + pluginName + "/pool"})
	if err != nil || resp.StatusCode != http.StatusOK || string(resp.Body) != resources.PoolPage || resp.Headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("resource = %+v, err = %v", resp, err)
	}
	for _, endpoint := range []string{"/quota-usage", "/quota"} {
		if got := callPoolManagement(t, m, http.MethodPost, endpoint, `{}`); got.StatusCode != http.StatusNotFound {
			t.Fatalf("legacy endpoint exposed: %s", endpoint)
		}
	}
}

func TestPoolManagementAccountsCRUDAndSafeCatalogFailure(t *testing.T) {
	m := newPoolHandlerManager(t)
	const secret = "management-test-secret"
	resp := callPoolManagement(t, m, http.MethodPost, "/accounts", `{"name":"First","group_id":"one","api_key":"`+secret+`","max_concurrency":2}`)
	if resp.StatusCode != http.StatusOK || strings.Contains(string(resp.Body), secret) {
		t.Fatalf("create = %d %s", resp.StatusCode, resp.Body)
	}
	var created struct {
		Account             pool.AccountView `json:"account"`
		CatalogRefreshError string           `json:"catalog_refresh_error"`
	}
	if json.Unmarshal(resp.Body, &created) != nil || created.Account.ID == "" || created.CatalogRefreshError == "" {
		t.Fatalf("write success was lost on catalog failure: %s", resp.Body)
	}
	resp = callPoolManagement(t, m, http.MethodGet, "/accounts", "")
	var listed struct {
		Accounts []pool.AccountView `json:"accounts"`
	}
	if json.Unmarshal(resp.Body, &listed) != nil || len(listed.Accounts) != 1 {
		t.Fatalf("list = %s", resp.Body)
	}
	body, _ := json.Marshal(pool.AccountInput{ID: created.Account.ID, Name: "Edited", GroupID: "one", MaxConcurrency: 2})
	if resp = callPoolManagement(t, m, http.MethodPost, "/accounts", string(body)); resp.StatusCode != http.StatusOK || m.pool.Snapshot()[0].Name != "Edited" {
		t.Fatalf("edit without key = %d %s", resp.StatusCode, resp.Body)
	}
	body, _ = json.Marshal(map[string]string{"id": created.Account.ID})
	resp = callPoolManagement(t, m, http.MethodPost, "/accounts/delete", string(body))
	if resp.StatusCode != http.StatusOK || len(m.pool.Snapshot()) != 0 || !strings.Contains(string(resp.Body), `"deleted":true`) {
		t.Fatalf("delete = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestPoolManagementStrictJSONAndImportPreflight(t *testing.T) {
	m := newPoolHandlerManager(t)
	valid := `{"name":"Valid","group_id":"g","api_key":"preflight-secret","max_concurrency":2}`
	for _, body := range []string{"", "null", "[]", valid + `{}`, strings.TrimSuffix(valid, "}") + `,"unknown":"secret"}`, `{"name":7}`} {
		resp := callPoolManagement(t, m, http.MethodPost, "/accounts", body)
		if resp.StatusCode != http.StatusBadRequest || len(m.pool.Snapshot()) != 0 {
			t.Fatalf("invalid JSON mutated pool: %q => %d %s", body, resp.StatusCode, resp.Body)
		}
	}
	for _, body := range []string{
		`{"accounts":[]}`, `{"accounts":null}`,
		`{"accounts":[` + valid + `,` + valid + `]}`,
		`{"accounts":[` + valid + `,{"name":"Bad","group_id":"g","api_key":"second-secret","extra":true}]}`,
		`{"accounts":[{"id":"same","name":"One"},{"id":" same ","name":"Two"}]}`,
	} {
		resp := callPoolManagement(t, m, http.MethodPost, "/accounts/import", body)
		if resp.StatusCode != http.StatusBadRequest || len(m.pool.Snapshot()) != 0 || strings.Contains(string(resp.Body), "secret") {
			t.Fatalf("preflight failure mutated pool or leaked input: %d %s", resp.StatusCode, resp.Body)
		}
	}
	resp := callPoolManagement(t, m, http.MethodPost, "/accounts", strings.Repeat(" ", poolManagementBodyLimit+1))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit = %d", resp.StatusCode)
	}
	if resp := callPoolManagement(t, m, http.MethodPost, "/accounts/delete", `{"id":"missing","api_key":"bad"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete unknown field accepted = %d", resp.StatusCode)
	}
	if resp := callPoolManagement(t, m, http.MethodPost, "/quota/refresh", `{"id":"a"} {}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("refresh trailing JSON accepted = %d", resp.StatusCode)
	}
}

func TestPoolManagementImportReportsPartialSuccess(t *testing.T) {
	m := newPoolHandlerManager(t)
	resp := callPoolManagement(t, m, http.MethodPost, "/accounts/import", `{"accounts":[{"name":"Good","group_id":"g","api_key":"partial-good","max_concurrency":2},{"name":"","group_id":"g","api_key":"partial-bad","max_concurrency":2}]}`)
	var got struct {
		Accounts       []pool.AccountView `json:"accounts"`
		Error          string             `json:"error"`
		PartialSuccess bool               `json:"partial_success"`
	}
	if json.Unmarshal(resp.Body, &got) != nil || resp.StatusCode != http.StatusOK || got.Error == "" || !got.PartialSuccess || len(got.Accounts) != 1 || len(m.pool.Snapshot()) != 1 {
		t.Fatalf("partial import misreported: %d %s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "partial-good") || strings.Contains(string(resp.Body), "partial-bad") {
		t.Fatalf("partial import leaked keys: %s", resp.Body)
	}
}

func TestPoolManagementImportDuplicateExistingTarget(t *testing.T) {
	m := newPoolHandlerManager(t)
	account := addPoolHandlerAccount(t, m, "existing-import-secret", "Original", "g")
	body, err := json.Marshal(map[string]any{"accounts": []pool.AccountInput{
		{ID: account.ID, Name: "First edit", GroupID: "g", MaxConcurrency: 2},
		{APIKey: "existing-import-secret", Name: "Second edit", GroupID: "g", MaxConcurrency: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	resp := callPoolManagement(t, m, http.MethodPost, "/accounts/import", string(body))
	if resp.StatusCode != http.StatusBadRequest || m.pool.Snapshot()[0].Name != "Original" {
		t.Fatalf("same account with ID/key was not rejected upfront: %d %s", resp.StatusCode, resp.Body)
	}
}

func TestPoolManagementQuotaRefreshEmptyAndSafeFailure(t *testing.T) {
	m := newPoolHandlerManager(t)
	if resp := callPoolManagement(t, m, http.MethodPost, "/quota/refresh", `{}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("empty pool refresh = %d %s", resp.StatusCode, resp.Body)
	}
	addPoolHandlerAccount(t, m, "refresh-management-secret", "Account", "g")
	m.cfg.BaseURL = "file://" + m.cfg.Pool.AuthDir + "/refresh-management-secret"
	resp := callPoolManagement(t, m, http.MethodPost, "/quota/refresh", `{}`)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(resp.Body), "refresh-management-secret") || strings.Contains(string(resp.Body), m.cfg.Pool.AuthDir) {
		t.Fatalf("unsafe refresh error = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestPoolManagementStatusEventsAndCursors(t *testing.T) {
	m := newPoolHandlerManager(t)
	addPoolHandlerAccount(t, m, "status-event-secret", "Account", "g")
	resp := callPoolManagement(t, m, http.MethodGet, "/status", "")
	var got struct {
		Status struct {
			Provider string `json:"provider_id"`
			Scope    string `json:"scope"`
			Count    int    `json:"account_count"`
			Limit    int    `json:"default_limit"`
			MaxAge   int    `json:"quota_max_age_seconds"`
			Refresh  int    `json:"quota_refresh_interval_seconds"`
			Home     bool   `json:"home_supported"`
		} `json:"status"`
	}
	if json.Unmarshal(resp.Body, &got) != nil || got.Status.Provider != ProviderID || got.Status.Scope != "single_process" || got.Status.Count != 1 || got.Status.Limit != 2 || got.Status.MaxAge != 60 || got.Status.Refresh != 10 || got.Status.Home {
		t.Fatalf("status = %s", resp.Body)
	}
	if strings.Contains(string(resp.Body), m.cfg.Pool.AuthDir) || strings.Contains(string(resp.Body), "status-event-secret") {
		t.Fatalf("status leaked storage or key: %s", resp.Body)
	}
	resp = callPoolManagement(t, m, http.MethodGet, "/events", "")
	var events struct {
		Events []pool.Event `json:"events"`
		Cursor uint64       `json:"next_cursor"`
	}
	if json.Unmarshal(resp.Body, &events) != nil || len(events.Events) == 0 || events.Cursor != events.Events[len(events.Events)-1].Sequence {
		t.Fatalf("events = %s", resp.Body)
	}
	for _, values := range []url.Values{{"after": {"-1"}}, {"after": {"18446744073709551616"}}, {"after": {"1", "2"}}, {"after": {""}}} {
		resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/events", Query: values})
		if err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid cursor = %d %v", resp.StatusCode, err)
		}
	}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/events", Query: url.Values{"after": {"18446744073709551615"}}})
	if err != nil || json.Unmarshal(resp.Body, &events) != nil || len(events.Events) != 0 || events.Cursor != ^uint64(0) {
		t.Fatalf("empty cursor advanced backwards: %s %v", resp.Body, err)
	}
}

func TestPoolManagementClosedAndCRUDLockOrder(t *testing.T) {
	m := newPoolHandlerManager(t)
	m.lifeMu.Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		_, _ = m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/" + pluginName + "/accounts", Body: []byte(`{}`)})
		close(done)
	}()
	<-started
	waited := make(chan struct{})
	go func() { m.managementWG.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		m.lifeMu.Unlock()
		t.Fatal("CRUD joined waitgroup before obtaining lifecycle lock")
	}
	// 非 CRUD 即使 lifeMu 被持有也必须能完成。
	readDone := make(chan pluginapi.ManagementResponse, 1)
	go func() {
		resp, _ := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/accounts"})
		readDone <- resp
	}()
	select {
	case resp := <-readDone:
		if resp.StatusCode != http.StatusOK {
			t.Errorf("read status = %d", resp.StatusCode)
		}
	case <-time.After(time.Second):
		t.Error("read-only management took lifecycle lock")
	}
	m.lifeMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CRUD did not resume")
	}
	if _, err := m.handleShutdown(); err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, "/accounts", ""}, {http.MethodPost, "/accounts", `{}`},
		{http.MethodPost, "/quota/refresh", `{}`},
	} {
		if resp := callPoolManagement(t, m, route.method, route.path, route.body); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("shutdown accepted request = %d", resp.StatusCode)
		}
	}
}
