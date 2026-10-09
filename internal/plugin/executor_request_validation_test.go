package plugin

import (
	"net/http"
	"testing"

	"commandcode-cpa-plugin/internal/errclass"
)

func TestExecutorLocalRequestErrorsAre400AndReleaseOwner(t *testing.T) {
	cases := []struct {
		name   string
		format string
		body   string
		class  errclass.Class
	}{
		{"malformed", "claude", `{"messages":`, errclass.ClassTranslation},
		{"unknown role", "claude", `{"max_tokens":16,"messages":[{"role":"robot","content":"bad"}]}`, errclass.ClassUnsupported},
		{"unsupported block", "claude", `{"max_tokens":16,"messages":[{"role":"user","content":[{"type":"unknown"}]}]}`, errclass.ClassUnsupported},
		{"unsupported source format", "bogus-format", ccRequestBody, errclass.ClassUnsupported},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			mode := "non-stream"
			if stream {
				mode = "stream"
			}
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				m, f := newExecManager(t)
				beforeDo := len(f.callsOf(testNativeHTTPDo))
				beforeStream := len(f.callsOf(testNativeHTTPDoStream))
				method := "executor.execute"
				body := execReqBody("commandcode/glm-5.3", tc.format, []byte(tc.body), false)
				if stream {
					method = "executor.execute_stream"
					body = execStreamReqBody("commandcode/glm-5.3", tc.format, []byte(tc.body), "invalid-down")
				}
				raw, err := m.HandleCall(method, body)
				if err != nil {
					t.Fatal(err)
				}
				env := decodeEnv(t, raw)
				if env.OK || env.Error == nil || env.Error.Code != string(tc.class) || env.Error.HTTPStatus != http.StatusBadRequest || env.Error.Retryable {
					t.Fatalf("local validation envelope = %s", raw)
				}
				if len(f.callsOf(testNativeHTTPDo)) != beforeDo || len(f.callsOf(testNativeHTTPDoStream)) != beforeStream {
					t.Fatal("invalid request reached upstream")
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

func TestRequestValidationEnvelopeDoesNotMutateSharedError(t *testing.T) {
	original := &errclass.Error{Class: errclass.ClassTranslation, Message: "bad request", Retryable: true}
	env := decodeEnv(t, requestValidationEnvelope(original))
	if env.Error == nil || env.Error.HTTPStatus != http.StatusBadRequest || env.Error.Retryable {
		t.Fatalf("request envelope = %+v", env.Error)
	}
	if original.StatusCode != 0 || !original.Retryable {
		t.Fatalf("shared error was mutated: %+v", original)
	}
	upstream := decodeEnv(t, classEnvelope(original))
	if upstream.Error == nil || upstream.Error.HTTPStatus != 0 || !upstream.Error.Retryable {
		t.Fatalf("upstream translation classification changed: %+v", upstream.Error)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		err := errclass.FromStatus(status, "synthetic failure")
		env := decodeEnv(t, requestValidationEnvelope(err))
		if env.Error == nil || env.Error.HTTPStatus != status || env.Error.Retryable != err.Retryable || env.Error.Code != string(err.Class) {
			t.Fatalf("explicit status %d changed: %+v", status, env.Error)
		}
	}
}
