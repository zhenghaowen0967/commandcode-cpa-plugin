package shared

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeClaudeHistoryRemindersAndToolOrder(t *testing.T) {
	body := []byte(`{"system":"top","messages":[
		{"role":"user","content":"start"},
		{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{"n":9007199254740993}},{"type":"tool_use","id":"b","name":"other","input":{}}]},
		{"role":"system","content":"first"},
		{"role":"system","content":[{"type":"text","text":"second"},{"type":"text","text":"third"}]},
		{"role":"user","content":[{"type":"text","text":"before"},{"type":"tool_result","tool_use_id":"b","content":"bad","is_error":true},{"type":"text","text":"after"},{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"ok"}]}]}
	]}`)
	record, eErr := DecodeClaudeMessages(body)
	if eErr != nil {
		t.Fatal(eErr)
	}
	before, _ := json.Marshal(record)
	messages, eErr := NormalizeClaudeHistory(record.Messages, "/v1/test")
	if eErr != nil {
		t.Fatal(eErr)
	}
	after, _ := json.Marshal(record)
	if string(before) != string(after) || record.System != "top" {
		t.Fatal("normalization mutated source or top-level system")
	}
	if len(messages) != 6 {
		t.Fatalf("messages: %+v", messages)
	}
	results := messages[2].Blocks
	if len(results) != 2 || results[0].CallID != "a" || results[1].CallID != "b" || !results[1].IsError || string(results[0].Result) != `[{"type":"text","text":"ok"}]` {
		t.Fatalf("tool results changed or unordered: %+v", results)
	}
	if got := string(messages[1].Blocks[0].Input); got != `{"n":9007199254740993}` {
		t.Fatalf("tool arguments changed: %s", got)
	}
	if messages[3].Role != "user" || messages[3].Content != "<system-reminder>\nfirst\n</system-reminder>" || messages[4].Content != "<system-reminder>\nsecond\nthird\n</system-reminder>" {
		t.Fatalf("reminders: %+v", messages[3:5])
	}
	if messages[5].Blocks[0].Text != "before" || messages[5].Blocks[1].Text != "after" {
		t.Fatalf("ordinary content lost: %+v", messages[5])
	}
}

func TestNormalizeClaudeHistoryReminderBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
	}{
		{"string", `"hint"`, "<system-reminder>\nhint\n</system-reminder>"},
		{"array", `[{"type":"text","text":"one"},{"type":"text","text":"two"}]`, "<system-reminder>\none\ntwo\n</system-reminder>"},
		{"empty", `""`, ""},
		{"whitespace", `[{"type":"text","text":"  \n"}]`, ""},
		{"null", `null`, ""},
		{"attribution", `"  x-anthropic-billing-header: cch=test;"`, ""},
		{"mixed attribution", `[{"type":"text","text":"\tx-anthropic-billing-header: cch=test;"},{"type":"text","text":"keep"}]`, "<system-reminder>\nkeep\n</system-reminder>"},
		{"embedded attribution", `"keep x-anthropic-billing-header: text"`, "<system-reminder>\nkeep x-anthropic-billing-header: text\n</system-reminder>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, eErr := DecodeClaudeMessages([]byte(`{"messages":[{"role":"system","content":` + tc.content + `}]}`))
			if eErr != nil {
				t.Fatal(eErr)
			}
			messages, eErr := NormalizeClaudeHistory(record.Messages, "/v1/test")
			if eErr != nil {
				t.Fatal(eErr)
			}
			if tc.want == "" {
				if len(messages) != 0 {
					t.Fatalf("empty reminder forwarded: %+v", messages)
				}
			} else if len(messages) != 1 || messages[0].Role != "user" || messages[0].Content != tc.want {
				t.Fatalf("reminder: %+v", messages)
			}
		})
	}
	for _, kind := range []string{"image", "tool_use", "thinking", "document"} {
		_, eErr := NormalizeClaudeHistory([]ClaudeMessageRecord{{Role: "system", Blocks: []ClaudeBlock{{Kind: kind}}}}, "/v1/test")
		if eErr == nil || !strings.Contains(eErr.Message, kind) || !strings.Contains(eErr.Message, "/v1/test") {
			t.Fatalf("unsupported reminder %s: %+v", kind, eErr)
		}
	}
}

func TestAlignClaudeHistoryIncompleteResultsPreserved(t *testing.T) {
	for _, results := range [][]ClaudeBlock{
		{{CallID: "b", Result: json.RawMessage(`"b"`)}},
		{{CallID: "b"}, {CallID: "unknown"}},
		{{CallID: "a"}, {CallID: "a"}},
	} {
		before, _ := json.Marshal(results)
		got := alignClaudeResults(results, []string{"a", "b"})
		after, _ := json.Marshal(got)
		if !reflect.DeepEqual(results, got) || string(before) != string(after) {
			t.Fatalf("incomplete results changed: %+v", got)
		}
	}
}

func TestNormalizeClaudeHistoryQueuedReminderNotLost(t *testing.T) {
	for _, tail := range [][]ClaudeMessageRecord{
		nil,
		{{Role: "user", Content: "continue"}},
		{{Role: "assistant", Content: "next"}},
	} {
		messages := []ClaudeMessageRecord{
			{Role: "assistant", Blocks: []ClaudeBlock{{Kind: "tool_use", CallID: "a"}}},
			{Role: "system", Content: "keep"},
		}
		messages = append(messages, tail...)
		got, eErr := NormalizeClaudeHistory(messages, "/v1/test")
		if eErr != nil || len(got) != 2+len(tail) || got[1].Content != "<system-reminder>\nkeep\n</system-reminder>" {
			t.Fatalf("queued reminder lost: %+v, %+v", got, eErr)
		}
	}
}
