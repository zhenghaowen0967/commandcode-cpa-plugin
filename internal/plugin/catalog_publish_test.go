package plugin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/pool"
)

func catalogRevisionFixture(t *testing.T, m *Manager, authID string) ([]byte, string) {
	t.Helper()
	m.mu.RLock()
	dir := m.cfg.Pool.AuthDir
	m.mu.RUnlock()
	raw, err := os.ReadFile(filepath.Join(dir, authID))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Revision string `json:"catalog_revision"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return raw, record.Revision
}

func assertCatalogRevision(t *testing.T, revision string) {
	t.Helper()
	nonce, err := hex.DecodeString(revision)
	if err != nil || len(nonce) != 32 {
		t.Fatalf("catalog revision must be a 32-byte hex nonce: %q", revision)
	}
}

func catalogForAuthFixture(t *testing.T, m *Manager, authID, provider string) pluginapi.ModelResponse {
	t.Helper()
	var response pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelForAuth, mustJSON(pluginapi.AuthModelRequest{AuthID: authID, AuthProvider: provider})), &response)
	return response
}

func TestCatalogPublicationColdImportPublishesWatchedAuthMetadata(t *testing.T) {
	m, f := newTestManager(catalogResponder(true, testCatalogJSON))
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, "api-keys: []\n"))
	t.Cleanup(func() { _, _ = m.handleShutdown() })
	var initial pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelStatic, nil), &initial)
	if len(initial.Models) != 0 || len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 0 {
		t.Fatalf("empty startup unexpectedly fetched or exposed models: %+v", initial.Models)
	}
	response := callPoolManagement(t, m, http.MethodPost, "/accounts/import", `{"accounts":[{"name":"Cold import","group_id":"cold","api_key":"catalog-cold-import-key","max_concurrency":2}]}`)
	var imported struct {
		Accounts     []pool.AccountView `json:"accounts"`
		CatalogError string             `json:"catalog_refresh_error"`
	}
	if json.Unmarshal(response.Body, &imported) != nil || response.StatusCode != http.StatusOK || len(imported.Accounts) != 1 || imported.CatalogError != "" {
		t.Fatalf("cold import failed: %d %s", response.StatusCode, response.Body)
	}
	account := imported.Accounts[0]
	raw, revision := catalogRevisionFixture(t, m, account.AuthID)
	assertCatalogRevision(t, revision)
	parsed, err := (authProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, FileName: account.AuthID, RawJSON: raw})
	if err != nil || !parsed.Handled || parsed.Auth.Metadata["catalog_revision"] != revision {
		t.Fatalf("watcher metadata did not carry catalog revision: %+v %v", parsed.Auth.Metadata, err)
	}
	models := catalogForAuthFixture(t, m, parsed.Auth.ID, parsed.Auth.Provider)
	if len(models.Models) != 1 || models.Models[0].ID != "commandcode/glm-5.3" {
		t.Fatalf("cold import did not become routable for auth: %+v", models.Models)
	}
	if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != 1 {
		t.Fatalf("cold import refresh calls = %d", len(f.callsOf(pluginabi.MethodHostHTTPDo)))
	}
}

func TestCatalogPublicationInitialAndFailedRefreshRevision(t *testing.T) {
	var fail atomic.Bool
	m, _ := newTestManager(func(method string, payload []byte) ([]byte, error) {
		return catalogResponder(!fail.Load(), testCatalogJSON)(method, payload)
	})
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	t.Cleanup(func() { _, _ = m.handleShutdown() })
	account := m.pool.Snapshot()[0]
	before, revision := catalogRevisionFixture(t, m, account.AuthID)
	assertCatalogRevision(t, revision)
	fail.Store(true)
	if err := m.refreshCatalogNow(context.Background()); err == nil {
		t.Fatal("failed refresh was reported as published")
	}
	after, afterRevision := catalogRevisionFixture(t, m, account.AuthID)
	if afterRevision != revision || !bytes.Equal(before, after) {
		t.Fatal("failed catalog refresh rewrote watched auth or revision")
	}
	if got := catalogForAuthFixture(t, m, account.AuthID, ProviderID); len(got.Models) != 1 {
		t.Fatalf("failed refresh discarded stale serving catalog: %+v", got.Models)
	}
}

func TestCatalogPublicationSuccessfulEmptyCatalogRepublishes(t *testing.T) {
	var body atomic.Value
	body.Store(testCatalogJSON)
	m, _ := newTestManager(func(method string, payload []byte) ([]byte, error) {
		return catalogResponder(true, body.Load().(string))(method, payload)
	})
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	t.Cleanup(func() { _, _ = m.handleShutdown() })
	account := m.pool.Snapshot()[0]
	_, beforeRevision := catalogRevisionFixture(t, m, account.AuthID)
	body.Store(`{"data":[]}`)
	if err := m.refreshCatalogNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, afterRevision := catalogRevisionFixture(t, m, account.AuthID)
	assertCatalogRevision(t, afterRevision)
	if afterRevision == beforeRevision {
		t.Fatal("successful empty catalog did not notify the watcher to remove old models")
	}
	if got := catalogForAuthFixture(t, m, account.AuthID, ProviderID); len(got.Models) != 0 {
		t.Fatalf("successful empty catalog kept stale auth models: %+v", got.Models)
	}
}

func TestCatalogPublicationPeriodicRefreshChangesRevision(t *testing.T) {
	secondTick, releaseTick := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var requests atomic.Int32
	m, _ := newTestManager(func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostHTTPDo && requests.Add(1) == 3 {
			close(secondTick)
			<-releaseTick
		}
		return catalogResponder(true, testCatalogJSON)(method, payload)
	})
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseTick) })
		_, _ = m.handleShutdown()
	})
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
	account := m.pool.Snapshot()[0]
	_, beforeRevision := catalogRevisionFixture(t, m, account.AuthID)
	m.mu.Lock()
	cfg, mgr := m.cfg, m.mgr
	stop, done := make(chan struct{}), make(chan struct{})
	m.stop, m.done = stop, done
	m.mu.Unlock()
	m.startRefreshLoop(cfg, mgr, time.Millisecond, stop, done)
	select {
	case <-secondTick:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic refresh did not reach second serialized tick")
	}
	// 第二次刷新被阻塞，保证第一次刷新及其发布已经完成，而不是依赖 sleep 竞态。
	_, afterRevision := catalogRevisionFixture(t, m, account.AuthID)
	assertCatalogRevision(t, afterRevision)
	if afterRevision == beforeRevision {
		t.Fatal("periodic refresh did not republish watched auth revision")
	}
	releaseOnce.Do(func() { close(releaseTick) })
	if oldDone := m.closeStop(); oldDone != nil {
		<-oldDone
	}
}

func TestCatalogForAuthRejectsUnknownDisabledAndWrongProvider(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	t.Cleanup(func() { _, _ = m.handleShutdown() })
	account := m.pool.Snapshot()[0]
	if got := catalogForAuthFixture(t, m, account.AuthID, ProviderID); len(got.Models) != 1 {
		t.Fatalf("enabled auth models = %+v", got.Models)
	}
	for _, input := range []struct{ id, provider string }{
		{"", ProviderID}, {"unknown", ProviderID}, {account.AuthID, "other"}, {account.AuthID, ""},
	} {
		if got := catalogForAuthFixture(t, m, input.id, input.provider); len(got.Models) != 0 || got.Models == nil {
			t.Fatalf("unknown/wrong-provider models = %+v", got.Models)
		}
	}
	disabled := false
	if _, err := m.pool.Upsert(pool.AccountInput{ID: account.ID, Name: account.Name, GroupID: account.GroupID, MaxConcurrency: account.MaxConcurrency, Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if got := catalogForAuthFixture(t, m, account.AuthID, ProviderID); len(got.Models) != 0 {
		t.Fatalf("disabled auth exposed models: %+v", got.Models)
	}
	var static pluginapi.ModelResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodModelStatic, nil), &static)
	if len(static.Models) != 1 {
		t.Fatalf("auth filtering changed static model catalog: %+v", static.Models)
	}
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodModelForAuth, []byte(`{"AuthID":`))); env.OK || env.Error == nil || env.Error.Code != "invalid_request" {
		t.Fatalf("malformed auth model request = %+v", env)
	}
}

func TestCatalogPublicationFailureIsSafeAndDoesNotAdvanceRevision(t *testing.T) {
	m, _ := newTestManager(catalogResponder(true, testCatalogJSON))
	mustHandle(t, m, pluginabi.MethodPluginRegister, lifecycleRequestBody(t, testValidYAML))
	t.Cleanup(func() { _, _ = m.handleShutdown() })
	account := m.pool.Snapshot()[0]
	_, revision := catalogRevisionFixture(t, m, account.AuthID)
	m.pool.Close()
	err := m.refreshCatalogNow(context.Background())
	if err == nil || err.Error() != "catalog publication failed" || strings.Contains(err.Error(), testKey) || strings.Contains(err.Error(), m.cfg.Pool.AuthDir) {
		t.Fatalf("unsafe or masked publication failure: %v", err)
	}
	_, afterRevision := catalogRevisionFixture(t, m, account.AuthID)
	if afterRevision != revision {
		t.Fatal("failed publication advanced persisted revision")
	}
}
