package shared

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeClaudeHistorySplitToolResults(t *testing.T) {
	for _, partial := range []bool{false, true} {
		messages := []ClaudeMessageRecord{
			{Role: "assistant", Blocks: []ClaudeBlock{{Kind: "tool_use", CallID: "a"}, {Kind: "tool_use", CallID: "b"}}},
			{Role: "system", Content: "hint"},
			{Role: "user", Blocks: []ClaudeBlock{{Kind: "text", Text: "before"}, {Kind: "tool_result", CallID: "b", Result: json.RawMessage(`"second"`)}}},
			{Role: "system", Content: "later"},
			{Role: "user", Content: "between"},
		}
		if !partial {
			messages = append(messages, ClaudeMessageRecord{Role: "user", Blocks: []ClaudeBlock{{Kind: "tool_result", CallID: "a", Result: json.RawMessage(`"first"`)}}})
		}
		before, _ := json.Marshal(messages)
		got, eErr := NormalizeClaudeHistory(messages, "/v1/test")
		if eErr != nil {
			t.Fatal(eErr)
		}
		after, _ := json.Marshal(messages)
		if string(before) != string(after) {
			t.Fatal("source mutated")
		}
		if len(got) != 6 {
			t.Fatalf("history: %+v", got)
		}
		if partial {
			if len(got[1].Blocks) != 1 || got[1].Blocks[0].CallID != "b" {
				t.Fatalf("partial result changed: %+v", got)
			}
		} else if len(got[1].Blocks) != 2 || got[1].Blocks[0].CallID != "a" || got[1].Blocks[1].CallID != "b" {
			t.Fatalf("split result ordering: %+v", got)
		}
		if !strings.Contains(got[2].Content, "hint") || got[3].Blocks[0].Text != "before" || !strings.Contains(got[4].Content, "later") || got[5].Content != "between" {
			t.Fatalf("deferred content order/loss: %+v", got)
		}
	}
}
