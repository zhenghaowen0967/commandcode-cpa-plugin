package shared_test

import (
	"encoding/json"
	"strings"
	"testing"

	"commandcode-cpa-plugin/internal/adapter/chatcompletions"
	"commandcode-cpa-plugin/internal/adapter/responses"
	"commandcode-cpa-plugin/internal/errclass"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestClaudeHistorySplitResultsBothTargets(t *testing.T) {
	for name, build := range map[string]func(string, string, []byte, *pluginapi.ThinkingSupport) ([]byte, *errclass.Error){
		"chat": chatcompletions.BuildRequest, "responses": responses.BuildRequest,
	} {
		for _, stream := range []string{"false", "true"} {
			body := []byte(`{"stream":` + stream + `,"messages":[
				{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"tool_use","id":"b","name":"g","input":{}}]},
				{"role":"system","content":"hint"},
				{"role":"user","content":[{"type":"text","text":"continue"},{"type":"tool_result","tool_use_id":"b","content":"second"}]},
				{"role":"system","content":"later"},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"first"}]}
			]}`)
			wire, eErr := build("upstream", "claude", body, nil)
			if eErr != nil {
				t.Fatal(eErr)
			}
			var out map[string]any
			if err := json.Unmarshal(wire, &out); err != nil {
				t.Fatal(err)
			}
			if name == "chat" {
				items := out["messages"].([]any)
				if len(items) != 6 || items[0].(map[string]any)["role"] != "assistant" {
					t.Fatalf("split chat: %s", wire)
				}
				for i, id := range []string{"a", "b"} {
					item := items[i+1].(map[string]any)
					if item["role"] != "tool" || item["tool_call_id"] != id {
						t.Fatalf("split chat adjacency: %s", wire)
					}
				}
				for i, text := range []string{"hint", "continue", "later"} {
					item := items[i+3].(map[string]any)
					if item["role"] != "user" || !strings.Contains(item["content"].(string), text) {
						t.Fatalf("split chat content: %s", wire)
					}
				}
			} else {
				items := out["input"].([]any)
				if len(items) != 7 {
					t.Fatalf("split responses: %s", wire)
				}
				for i, id := range []string{"a", "b"} {
					item := items[i+2].(map[string]any)
					if item["type"] != "function_call_output" || item["call_id"] != id {
						t.Fatalf("split responses adjacency: %s", wire)
					}
				}
				for i, text := range []string{"hint", "continue", "later"} {
					item := items[i+4].(map[string]any)
					if item["role"] != "user" || !strings.Contains(item["content"].([]any)[0].(map[string]any)["text"].(string), text) {
						t.Fatalf("split responses content: %s", wire)
					}
				}
			}
		}
	}
}
