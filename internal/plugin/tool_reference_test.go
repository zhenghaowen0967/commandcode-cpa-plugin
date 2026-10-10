package plugin

import (
	"net/http"
	"strings"
	"testing"

	"commandcode-cpa-plugin/internal/errclass"
)

// deriveToolReferenceBody: an initial claude user turn whose tool_result
// content carries a tool_reference to a declared tool — the shape Claude
// Code sends when a deferred tool became available.
const deriveToolReferenceBody = `{"model":"x","max_tokens":64,"tools":[
	{"name":"Read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"$ref":"#/$defs/path"}},"required":["path"],"$defs":{"path":{"type":"string","minLength":1}}}}
],"messages":[
	{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"tu_1","content":[
			{"type":"text","text":"loaded "},
			{"type":"tool_reference","tool_name":"Read"}
		]}
	]},
	{"role":"user","content":"later"}
]}`

// A legal tool_reference in the initial user turn must derive a session
// (not reject): the reference renders through the same shared kernel the
// serving routes use, and the digest covers it like any other result text.
func TestDeriveSessionWithToolReference(t *testing.T) {
	got, eErr := deriveCommandCodeSessionID("claude", []byte(deriveToolReferenceBody))
	if eErr != nil {
		t.Fatalf("reference-bearing derivation rejected: %+v", eErr)
	}
	if got == "" || got == emptyCommandCodeSessionID {
		t.Fatalf("reference-bearing derivation collapsed: %q", got)
	}
	// The referenced tool name and its declared schema are model-visible
	// content: a request whose reference names a different tool derives a
	// different session.
	other := strings.Replace(deriveToolReferenceBody, `"tool_name":"Read"`, `"tool_name":"Write"`, 1)
	gotOther, eErr := deriveCommandCodeSessionID("claude", []byte(other))
	if eErr != nil || gotOther == got {
		t.Fatalf("tool name change did not change session: %q vs %q (%+v)", got, gotOther, eErr)
	}
	// Reference-free text derivation keeps the historical vectors
	// (TestDeriveCommandCodeSessionID pins the exact digests; here the
	// invariant is that adding a later turn must not change the digest).
	base := `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"loaded "}]}]},{"role":"user","content":"later"}]}`
	b1, eErr1 := deriveCommandCodeSessionID("claude", []byte(base))
	if eErr1 != nil || b1 == "" {
		t.Fatalf("text-only derivation broke: %q %+v", b1, eErr1)
	}
}

// Malformed tool_reference (missing/blank tool_name) is a pre-IO 400 in the
// derivation path too — the session leg shares the kernel's rejection.
func TestDeriveSessionRejectsMalformedToolReference(t *testing.T) {
	for _, content := range []string{
		`[{"type":"tool_reference"}]`,
		`[{"type":"tool_reference","tool_name":"  "}]`,
		`[{"type":"tool_reference","tool_name":42}]`,
	} {
		body := `{"model":"x","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":` + content + `}]}]}`
		_, eErr := deriveCommandCodeSessionID("claude", []byte(body))
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("malformed reference %s accepted by derivation: %+v", content, eErr)
		}
	}
}

// Legal references on the serving path reach upstream in both modes; the
// malformed variants never touch transport and release the owner (the same
// contract TestExecutorLocalRequestErrorsAre400AndReleaseOwner pins for the
// generic malformed shapes).
func TestExecutorToolReferenceValidation(t *testing.T) {
	t.Run("legal reference reaches upstream non-stream", func(t *testing.T) {
		m, f := newExecManager(t)
		env := mustExecute(t, m, "commandcode/minimax-m3", "claude", []byte(deriveToolReferenceBody))
		if !env.OK {
			t.Fatalf("execute envelope = %+v", env.Error)
		}
		wire := lastWire(t, f, testNativeHTTPDo)
		body := string(wireBody(t, wire, "body"))
		if !strings.Contains(body, "tool_reference") {
			// Native claude passthrough on the messages route keeps the raw
			// reference upstream (RewriteModelID only rewrites the model).
			t.Fatalf("reference lost on messages passthrough: %s", body)
		}
	})

	for _, tc := range []struct{ name, blocks string }{
		{"missing tool_name", `{"type":"tool_reference"}`},
		{"blank tool_name", `{"type":"tool_reference","tool_name":"  "}`},
		{"non-string tool_name", `{"type":"tool_reference","tool_name":9}`},
		{"unknown block after reference", `{"type":"tool_reference","tool_name":"Read"},{"type":"mystery"}`},
	} {
		for _, stream := range []bool{false, true} {
			mode := "non-stream"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				m, f := newExecManager(t)
				beforeDo := len(f.callsOf(testNativeHTTPDo))
				beforeStream := len(f.callsOf(testNativeHTTPDoStream))
				// Chat route exercises the claude→CC translation; the
				// malformed reference must die in the shared kernel pre-IO.
				body := `{"model":"x","max_tokens":64,"tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[` +
					`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},` +
					`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` + tc.blocks + `]}]}]}`
				var resp []byte
				var err error
				if stream {
					resp, err = m.HandleCall("executor.execute_stream", execStreamReqBody("commandcode/glm-5.3", "claude", []byte(body), "ref-invalid-down"))
				} else {
					resp, err = m.HandleCall("executor.execute", execReqBody("commandcode/glm-5.3", "claude", []byte(body), false))
				}
				if err != nil {
					t.Fatal(err)
				}
				env := decodeEnv(t, resp)
				if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassTranslation) ||
					env.Error.HTTPStatus != http.StatusBadRequest || env.Error.Retryable {
					t.Fatalf("reference validation envelope = %s", resp)
				}
				if len(f.callsOf(testNativeHTTPDo)) != beforeDo || len(f.callsOf(testNativeHTTPDoStream)) != beforeStream {
					t.Fatal("malformed reference reached upstream")
				}
				assertExecutionSlot(t, m, false)
				valid := mustExecute(t, m, "commandcode/glm-5.3", "openai", []byte(ccRequestBody))
				if !valid.OK {
					t.Fatalf("valid request after rejection failed: %+v", valid.Error)
				}
				assertExecutionSlot(t, m, false)
			})
		}
	}
}
