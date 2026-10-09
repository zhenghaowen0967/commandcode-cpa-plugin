package shared_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"commandcode-cpa-plugin/internal/adapter/chatcompletions"
	"commandcode-cpa-plugin/internal/adapter/responses"
	"commandcode-cpa-plugin/internal/errclass"
)

func TestClaudeHistoryBothTargets(t *testing.T) {
	targets := []struct {
		name, endpoint string
		build          func(string, string, []byte, *pluginapi.ThinkingSupport) ([]byte, *errclass.Error)
	}{
		{"chat", "/v1/chat/completions", chatcompletions.BuildRequest},
		{"responses", "/v1/responses", responses.BuildRequest},
	}
	for _, target := range targets {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", target.name, streaming), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"system":[{"type":"text","text":"top"},{"type":"text","text":"authority"}],"stream":%v,"messages":[
					{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{"n":9007199254740993}},{"type":"tool_use","id":"b","name":"other","input":{}}]},
					{"role":"system","content":"remind"},
					{"role":"user","content":[{"type":"text","text":"continue"},{"type":"tool_result","tool_use_id":"b","content":"bad","is_error":true},{"type":"tool_result","tool_use_id":"a","content":"ok"}]}
				]}`, streaming))
				wire, eErr := target.build("upstream", "claude", body, nil)
				if eErr != nil {
					t.Fatal(eErr)
				}
				var out map[string]any
				if err := json.Unmarshal(wire, &out); err != nil {
					t.Fatal(err)
				}
				if out["model"] != "upstream" || (out["stream"] == true) != streaming {
					t.Fatalf("request controls changed: %s", wire)
				}
				if target.name == "chat" {
					items := out["messages"].([]any)
					roles := make([]string, 0, len(items))
					for _, item := range items {
						roles = append(roles, item.(map[string]any)["role"].(string))
					}
					if !reflect.DeepEqual(roles, []string{"system", "assistant", "tool", "tool", "user", "user"}) || items[0].(map[string]any)["content"] != "top\n\nauthority" {
						t.Fatalf("tool adjacency/top-level system: %s", wire)
					}
					for i, id := range []string{"a", "b"} {
						if items[i+2].(map[string]any)["tool_call_id"] != id {
							t.Fatalf("result order: %s", wire)
						}
					}
					if items[3].(map[string]any)["content"] != "[error] bad" || items[4].(map[string]any)["content"] != "<system-reminder>\nremind\n</system-reminder>" || items[5].(map[string]any)["content"] != "continue" {
						t.Fatalf("content changed: %s", wire)
					}
					calls := items[1].(map[string]any)["tool_calls"].([]any)
					if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"n":9007199254740993}` {
						t.Fatalf("arguments: %s", wire)
					}
				} else {
					items := out["input"].([]any)
					if out["instructions"] != "top\n\nauthority" || len(items) != 6 {
						t.Fatalf("instructions/input: %s", wire)
					}
					for i, id := range []string{"a", "b"} {
						call, result := items[i].(map[string]any), items[i+2].(map[string]any)
						if call["type"] != "function_call" || result["type"] != "function_call_output" || call["call_id"] != id || result["call_id"] != id {
							t.Fatalf("tool adjacency/order: %s", wire)
						}
					}
					if items[0].(map[string]any)["arguments"] != `{"n":9007199254740993}` || items[3].(map[string]any)["output"] != "[error] bad" {
						t.Fatalf("tool payload: %s", wire)
					}
					for i, want := range []string{"<system-reminder>\nremind\n</system-reminder>", "continue"} {
						message := items[i+4].(map[string]any)
						if message["role"] != "user" || message["content"].([]any)[0].(map[string]any)["text"] != want {
							t.Fatalf("reminder/content: %s", wire)
						}
					}
				}
			})
		}
		for _, body := range []string{
			`{"messages":[{"role":"developer","content":"bad"}]}`,
			`{"messages":[{"role":"system","content":[{"type":"document"}]}]}`,
			`{"messages":[{"role":"user","content":[{"type":"unknown"}]}]}`,
		} {
			_, eErr := target.build("upstream", "claude", []byte(body), nil)
			if eErr == nil || eErr.Class != errclass.ClassUnsupported || !strings.Contains(eErr.Message, target.endpoint) {
				t.Fatalf("%s invalid history accepted: %+v", target.name, eErr)
			}
		}
	}
}
