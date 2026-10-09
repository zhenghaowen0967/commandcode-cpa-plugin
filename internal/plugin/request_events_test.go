package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"commandcode-cpa-plugin/internal/pool"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPoolManagementRequestEventScope(t *testing.T) {
	m := newPoolHandlerManager(t)
	addPoolHandlerAccount(t, m, "request-scope-secret", "Account", "g")
	m.pool.Pick(nil, "request-1", "model")
	m.pool.AbortRequest("other-provider")
	for _, query := range []url.Values{{"scope": {""}}, {"scope": {"unknown"}}, {"scope": {"requests", "requests"}}, {"scope": {"all"}}} {
		resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/events", Query: query})
		if err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid scope accepted: %v", query)
		}
	}
	for _, scoped := range []bool{false, true} {
		query := url.Values{}
		if scoped {
			query.Set("scope", "requests")
		}
		resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/events", Query: query})
		var got struct {
			Events []pool.Event `json:"events"`
			Cursor uint64       `json:"next_cursor"`
		}
		if err != nil || resp.StatusCode != 200 || json.Unmarshal(resp.Body, &got) != nil {
			t.Fatal("scope response failed")
		}
		want := 3
		if scoped {
			want = 1
		}
		if len(got.Events) != want || got.Cursor != got.Events[len(got.Events)-1].Sequence {
			t.Fatalf("scoped=%v: %+v", scoped, got)
		}
		if scoped && got.Events[0].Action != "pick" {
			t.Fatal("request filter wrong")
		}
	}
	resp, err := m.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginName + "/events", Query: url.Values{"scope": {"requests"}, "after": {"18446744073709551615"}}})
	var got struct {
		Events []pool.Event `json:"events"`
		Cursor uint64       `json:"next_cursor"`
	}
	if err != nil || json.Unmarshal(resp.Body, &got) != nil || len(got.Events) != 0 || got.Cursor != ^uint64(0) {
		t.Fatal("request max cursor changed")
	}
}
