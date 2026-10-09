package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthProviderParseAndRefresh(t *testing.T) {
	secret := "sk-auth-secret-1"
	p := authProvider{}
	rawBytes := []byte(`{"type":"commandcode-pool","id":"stable","label":"Primary","api_key":"` + secret + `"}`)
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID, FileName: "key.json",
		RawJSON: rawBytes,
	})
	if err != nil || !parsed.Handled || parsed.Auth.ID != "key.json" || parsed.Auth.Label != "Primary" || parsed.Auth.Attributes["api_key"] != secret || string(parsed.Auth.StorageJSON) != string(rawBytes) {
		t.Fatalf("parsed auth = %#v, err=%v", parsed, err)
	}
	if _, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"other","api_key":"x1"}`)}); err != nil {
		t.Fatalf("unrelated auth error = %v", err)
	}
	invalid, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "dispatch.json", RawJSON: []byte(`{"type":"commandcode-pool"}`)})
	if err == nil || invalid.Handled || strings.Contains(err.Error(), secret) {
		t.Fatalf("invalid auth = %#v, err=%v", invalid, err)
	}
	refreshed, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "stable", AuthProvider: ProviderID, Attributes: parsed.Auth.Attributes})
	if err != nil || refreshed.Auth.Attributes["api_key"] != secret {
		t.Fatalf("refresh = %#v, err=%v", refreshed, err)
	}
}

func TestAuthProviderAliasesAndFilenameFallback(t *testing.T) {
	p := authProvider{}
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "custom-file.json",
		RawJSON:  []byte(`{"provider":"commandcode-pool","api_key":"sk-alias-key"}`),
	})
	if err != nil || !parsed.Handled || parsed.Auth.ID != "custom-file.json" || parsed.Auth.Attributes["api_key"] != "sk-alias-key" {
		t.Fatalf("alias parse = %#v, err=%v", parsed, err)
	}

	unhandled, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		FileName: "other.json",
		RawJSON:  []byte(`{"provider":"other-provider","api_key":"sk-other-key"}`),
	})
	if err != nil || unhandled.Handled {
		t.Fatalf("unhandled parse = %#v, err=%v", unhandled, err)
	}
}

func TestAuthProviderMalformedRecognizedInput(t *testing.T) {

	p := authProvider{}
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		Provider: ProviderID,
		RawJSON:  []byte(`{"type":"commandcode-pool","api_key":"sk-malformed-secret-1"`),
	})
	if err == nil || parsed.Handled || strings.Contains(err.Error(), "sk-malformed-secret-1") {
		t.Fatalf("malformed recognized auth = %#v, err=%v", parsed, err)
	}
	if err.Error() != "commandcode auth record has invalid JSON" {
		t.Fatalf("malformed recognized auth error = %v", err)
	}

	unrelated, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{
		RawJSON: []byte(`{"type":"commandcode-pool","api_key":"sk-malformed-secret-2"`),
	})
	if err != nil || unrelated.Handled {
		t.Fatalf("malformed unrelated auth = %#v, err=%v", unrelated, err)
	}
}

func TestAuthDispatchMethods(t *testing.T) {
	m := NewManager(nil)
	var got pluginapi.AuthParseResponse
	raw, err := m.HandleCall(pluginabi.MethodAuthIdentifier, nil)
	if err != nil || !bytesContain(raw, []byte(ProviderID)) {
		t.Fatalf("identifier = %s, err=%v", raw, err)
	}
	raw, err = m.HandleCall(pluginabi.MethodAuthParse, mustJSON(pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "dispatch.json", RawJSON: []byte(`{"type":"commandcode-pool","api_key":"sk-dispatch-1"}`)}))
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, raw, &got)
	if !got.Handled || got.Auth.Attributes["api_key"] != "sk-dispatch-1" {
		t.Fatalf("parse dispatch = %#v", got)
	}
	for _, method := range []string{pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll} {
		raw, _ = m.HandleCall(method, []byte(`{}`))
		if env := decodeEnv(t, raw); env.OK || env.Error == nil || env.Error.Code != "unsupported" {
			t.Fatalf("%s = %#v", method, env.Error)
		}
	}
	raw, err = m.HandleCall(pluginabi.MethodAuthRefresh, mustJSON(struct{ pluginapi.AuthRefreshRequest }{pluginapi.AuthRefreshRequest{AuthID: "id", AuthProvider: ProviderID, Attributes: map[string]string{"api_key": "sk-refresh-1"}}}))
	if err != nil {
		t.Fatal(err)
	}
	var refreshed pluginapi.AuthRefreshResponse
	decodeResult(t, raw, &refreshed)
	if refreshed.Auth.ID != "id" || refreshed.Auth.Attributes["api_key"] != "sk-refresh-1" {
		t.Fatalf("refresh dispatch = %#v", refreshed)
	}
}

func TestAuthProviderCatalogRevisionAndDisabledRefresh(t *testing.T) {
	p := authProvider{}
	revision := strings.Repeat("ab", 32)
	raw := mustJSON(map[string]any{"type": ProviderID, "label": "Disabled account", "group_id": "g", "disabled": true, "email": "owner@example.test", "catalog_revision": revision})
	parsed, err := p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "disabled.json", RawJSON: raw})
	if err != nil || !parsed.Handled || !parsed.Auth.Disabled || parsed.Auth.Metadata["email"] != "owner@example.test" || parsed.Auth.Metadata["catalog_revision"] != revision {
		t.Fatalf("catalog metadata overwrote email or disabled state: %+v %v", parsed.Auth, err)
	}
	parsed.Auth.Metadata["existing_context"] = "keep"
	refreshed, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: parsed.Auth.ID, AuthProvider: parsed.Auth.Provider, StorageJSON: parsed.Auth.StorageJSON, Metadata: parsed.Auth.Metadata, Attributes: parsed.Auth.Attributes})
	if err != nil || !refreshed.Auth.Disabled || string(refreshed.Auth.StorageJSON) != string(raw) || refreshed.Auth.Metadata["catalog_revision"] != revision || refreshed.Auth.Metadata["email"] != "owner@example.test" || refreshed.Auth.Metadata["existing_context"] != "keep" {
		t.Fatalf("refresh changed disabled/catalog metadata: %+v %v", refreshed.Auth, err)
	}
	if _, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthProvider: ProviderID, StorageJSON: []byte(`{"disabled":true,"api_key":"fake-secret"`)}); err == nil || strings.Contains(err.Error(), "fake-secret") {
		t.Fatalf("malformed stored auth enabled or leaked a credential: %v", err)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func bytesContain(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
