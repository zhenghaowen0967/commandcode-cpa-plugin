// F5 route parity for tool_reference blocks: the same claude-source
// tool_result carrying text and tool_reference blocks must render through
// the shared ToolResultText kernel identically on the Chat Completions and
// Responses targets — same reference text, same tool-use id pairing, same
// content order, same is_error handling — and both keep the declared tool
// definitions untouched. Malformed references fail both legs identically.
package adapters

import (
	"encoding/json"
	"strings"
	"testing"

	"commandcode-cpa-plugin/internal/adapter/chatcompletions"
	"commandcode-cpa-plugin/internal/adapter/responses"
	"commandcode-cpa-plugin/internal/errclass"
)

// claudeToolReferenceParityBody is one logical turn pair: an assistant
// tool_use (Read) followed by a user tool_result whose content mixes text
// and references. Blocks vary per case via toolResultBlocks.
func claudeToolReferenceParityBody(blocks string) []byte {
	return []byte(`{"model":"x","max_tokens":64,"tools":[` +
		`{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"$ref":"#/$defs/path"}},"required":["path"],"$defs":{"path":{"type":"string","minLength":1}}}}` +
		`],` +
		`"messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"tu_ref","name":"Read","input":{"path":"a.go"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_ref","content":[` + blocks + `]}]}` +
		`]}`)
}

// chatToolResult returns the single role:"tool" message content produced by
// the Chat Completions target.
func chatToolResult(t *testing.T, body []byte) string {
	t.Helper()
	out, eErr := chatcompletions.BuildRequest("m", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("chat build: %v", eErr)
	}
	var m struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("chat output decode: %v", err)
	}
	for _, msg := range m.Messages {
		if msg.Role == "tool" {
			if msg.ToolCallID != "tu_ref" {
				t.Fatalf("chat tool_call_id = %q", msg.ToolCallID)
			}
			return msg.Content
		}
	}
	t.Fatalf("no tool message in chat output: %s", out)
	return ""
}

// respToolOutput returns the single function_call_output output string
// produced by the Responses target.
func respToolOutput(t *testing.T, body []byte) string {
	t.Helper()
	out, eErr := responses.BuildRequest("m", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("responses build: %v", eErr)
	}
	var m struct {
		Input []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Output  string `json:"output"`
		} `json:"input"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("responses output decode: %v", err)
	}
	for _, item := range m.Input {
		if item.Type == "function_call_output" {
			if item.CallID != "tu_ref" {
				t.Fatalf("responses call_id = %q", item.CallID)
			}
			return item.Output
		}
	}
	t.Fatalf("no function_call_output in responses output: %s", out)
	return ""
}

func TestToolReferenceParityAcrossTargets(t *testing.T) {
	cases := []struct {
		name   string
		blocks string
	}{
		{"declared reference", `{"type":"tool_reference","tool_name":"Read"}`},
		{"undeclared reference", `{"type":"tool_reference","tool_name":"Grep"}`},
		{"mixed text and reference", `{"type":"text","text":"pre "},{"type":"tool_reference","tool_name":"Read"},{"type":"text","text":" post"}`},
		{"plain text only", `{"type":"text","text":"plain result"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := claudeToolReferenceParityBody(tc.blocks)
			chat := chatToolResult(t, body)
			resp := respToolOutput(t, body)
			if chat != resp {
				t.Fatalf("target divergence:\nchat     = %q\nresponses= %q", chat, resp)
			}
			if tc.name == "plain text only" && chat != "plain result" {
				t.Fatalf("text-only rendering changed: %q", chat)
			}
			if strings.Contains(chat, "tu_ref") {
				t.Fatalf("reference leaked the tool_use id: %q", chat)
			}
		})
	}
}

// The declared tool definition (name, description, raw schema with
// refs/$defs) forwards identically on both targets: the reference is extra
// result data and must not alter the tools the request already declared.
func TestToolReferenceDoesNotAlterDeclaredTools(t *testing.T) {
	body := claudeToolReferenceParityBody(`{"type":"tool_reference","tool_name":"Read"}`)

	out, eErr := chatcompletions.BuildRequest("m", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("chat build: %v", eErr)
	}
	var chatOut struct {
		Tools []struct {
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Parameters  any    `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &chatOut); err != nil {
		t.Fatalf("chat decode: %v", err)
	}
	if len(chatOut.Tools) != 1 || chatOut.Tools[0].Function.Name != "Read" ||
		chatOut.Tools[0].Function.Description != "Read a file" {
		t.Fatalf("chat declared tools changed: %v", chatOut.Tools)
	}
	params, _ := json.Marshal(chatOut.Tools[0].Function.Parameters)
	if !strings.Contains(string(params), `$defs`) {
		t.Fatalf("chat tool schema lost refs: %s", params)
	}

	out, eErr = responses.BuildRequest("m", "claude", body, nil)
	if eErr != nil {
		t.Fatalf("responses build: %v", eErr)
	}
	var respOut struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &respOut); err != nil {
		t.Fatalf("responses decode: %v", err)
	}
	if len(respOut.Tools) != 1 || respOut.Tools[0].Name != "Read" ||
		respOut.Tools[0].Description != "Read a file" {
		t.Fatalf("responses declared tools changed: %v", respOut.Tools)
	}
	params, _ = json.Marshal(respOut.Tools[0].Parameters)
	if !strings.Contains(string(params), `$defs`) {
		t.Fatalf("responses tool schema lost refs: %s", params)
	}
}

// Malformed references (missing/blank/non-string tool_name, null block)
// fail both targets with ClassTranslation before any upstream transport.
func TestToolReferenceMalformedFailsBothTargets(t *testing.T) {
	for _, blocks := range []string{
		`{"type":"tool_reference"}`,
		`{"type":"tool_reference","tool_name":"  "}`,
		`{"type":"tool_reference","tool_name":7}`,
		`{"type":"tool_reference","tool_name":"Read"},null`,
		`{"type":"image","source":{}}`,
	} {
		body := claudeToolReferenceParityBody(blocks)
		_, chatErr := chatcompletions.BuildRequest("m", "claude", body, nil)
		_, respErr := responses.BuildRequest("m", "claude", body, nil)
		if chatErr == nil || chatErr.Class != errclass.ClassTranslation {
			t.Fatalf("chat accepted malformed %s: %+v", blocks, chatErr)
		}
		if respErr == nil || respErr.Class != errclass.ClassTranslation {
			t.Fatalf("responses accepted malformed %s: %+v", blocks, respErr)
		}
		// Each route keeps its own targetNoun wording by design; the
		// rejection must name the malformed reference, block type, or
		// block-array shape on both legs.
		names := func(msg string) bool {
			return strings.Contains(msg, "tool_reference") ||
				strings.Contains(msg, "unsupported tool_result block type") ||
				strings.Contains(msg, "must be a string or an array of blocks")
		}
		if !names(chatErr.Message) || !names(respErr.Message) {
			t.Fatalf("malformed rejection stopped naming the malformed block:\nchat     = %v\nresponses= %v", chatErr, respErr)
		}
	}
}
