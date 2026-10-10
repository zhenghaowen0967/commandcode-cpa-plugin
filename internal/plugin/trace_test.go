package plugin

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const trustedTestTrace = "018f0000-0000-7000-8000-000000000001"
const spoofedTestTrace = "018f0000-0000-7000-8000-000000000002"

func TestTraceInterceptorsOnlyUseTypedHostIdentity(t *testing.T) {
	m := newPoolHandlerManager(t)
	a := addPoolHandlerAccount(t, m, "synthetic-trace-hook", "账号", "owner")
	for _, trace := range []string{trustedTestTrace, "", " opaque", "opaque", trustedTestTrace + "\r\n"} {
		req := pluginapi.RequestInterceptRequest{
			RequestID: "trusted-host", TraceID: trace,
			Headers:  http.Header{traceIDHeader: {spoofedTestTrace}, strings.ToLower(traceIDHeader): {spoofedTestTrace}},
			Metadata: map[string]any{selectedPoolAuthMetadataKey: a.AuthID, "trace_id": spoofedTestTrace, "request_id": spoofedTestTrace},
		}
		for _, method := range []string{pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter} {
			var got pluginapi.RequestInterceptResponse
			decodeResult(t, mustHandle(t, m, method, poolHookBody(t, req)), &got)
			if got.Terminate || len(got.ClearHeaders) != 2 || got.ClearHeaders[1] != traceIDHeader || poolHookRequestID(got.Headers) != "trusted-host" {
				t.Fatalf("可信身份或头清理不符: %+v", got)
			}
			want := ""
			if trace == trustedTestTrace {
				want = trace
			}
			if poolHookTraceID(got.Headers) != want {
				t.Fatalf("回退了客户端 trace 或丢失宿主 trace: %+v", got.Headers)
			}
		}
		if len(req.Headers) != 2 {
			t.Fatal("拦截器修改了只读输入")
		}
	}
	var got pluginapi.RequestInterceptResponse
	req := pluginapi.RequestInterceptRequest{RequestID: "trusted-host", TraceID: trustedTestTrace, Metadata: map[string]any{selectedPoolAuthMetadataKey: "unrelated"}}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptAfter, poolHookBody(t, req)), &got)
	if len(got.Headers) != 0 || len(got.ClearHeaders) != 2 || got.Terminate {
		t.Fatal("追踪字段侵入了非池渠道")
	}
}

func TestTracePrivateHeaderRejectsAmbiguousOrMalformedValues(t *testing.T) {
	for _, headers := range []map[string][]string{
		{traceIDHeader: {trustedTestTrace, spoofedTestTrace}},
		{traceIDHeader: {trustedTestTrace}, strings.ToLower(traceIDHeader): {spoofedTestTrace}},
		{traceIDHeader: {" " + trustedTestTrace}},
		{traceIDHeader: {"opaque secret text"}},
	} {
		if poolHookTraceID(headers) != "" {
			t.Fatalf("接受了歧义或异常 trace: %+v", headers)
		}
	}
	if poolHookTraceID(map[string][]string{strings.ToLower(traceIDHeader): {trustedTestTrace}}) != trustedTestTrace {
		t.Fatal("标准大小写不敏感头被丢弃")
	}
}

func TestTraceFromHostThroughSchedulerExecutorAndNaturalSettle(t *testing.T) {
	m, _ := newNativeExecutionManager(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(requestIDHeader) != "" || r.Header.Get(traceIDHeader) != "" {
			t.Error("内部关联头被转发上游")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(ccResponseBody))
	})
	intercept := pluginapi.RequestInterceptRequest{RequestID: "host-execution-one", TraceID: trustedTestTrace, Metadata: map[string]any{selectedPoolAuthMetadataKey: testPoolAuthID(testKey)}}
	var trusted pluginapi.RequestInterceptResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptBefore, poolHookBody(t, intercept)), &trusted)
	pick := pluginapi.SchedulerPickRequest{Provider: ProviderID, Model: "commandcode/glm-5.3", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: testPoolAuthID(testKey), Provider: ProviderID}}, Options: pluginapi.SchedulerOptions{Headers: trusted.Headers}}
	var selected pluginapi.SchedulerPickResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodSchedulerPick, poolHookBody(t, pick)), &selected)
	if selected.AuthID != testPoolAuthID(testKey) {
		t.Fatalf("选号失败: %+v", selected)
	}
	decodeResult(t, mustHandle(t, m, pluginabi.MethodRequestInterceptAfter, poolHookBody(t, intercept)), &trusted)
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: selected.AuthID, AuthProvider: ProviderID, Model: pick.Model, SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody), Headers: trusted.Headers}}
	response := mustHandle(t, m, pluginabi.MethodExecutorExecute, poolHookBody(t, req))
	if !decodeEnv(t, response).OK {
		t.Fatalf("执行失败: %s", response)
	}
	events := m.pool.RequestEvents(0)
	if len(events) != 3 {
		t.Fatalf("期待 pick/acquire/settle: %+v", events)
	}
	for _, event := range events {
		if event.RequestID != intercept.RequestID || event.TraceID != trustedTestTrace {
			t.Fatalf("逐条关联丢失: %+v", event)
		}
	}
	if events[2].Action != "settle" || events[2].Reason != "upstream_complete" || m.pool.Snapshot()[0].Inflight != 0 {
		t.Fatal("自然释放合同发生回归")
	}
	if len(req.Headers) != 2 {
		t.Fatal("执行器修改了只读宿主请求头")
	}
}

func TestExecutorStripsTraceWithoutUsingItAsRequestID(t *testing.T) {
	m, _ := newNativeExecutionManager(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("缺 host ID 不应开始上游 I/O")
	})
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{AuthID: testPoolAuthID(testKey), AuthProvider: ProviderID, Model: "commandcode/glm-5.3", Headers: http.Header{traceIDHeader: {trustedTestTrace}}}}
	_, failure := m.acquireExecution(&req)
	if failure == nil || decodeEnv(t, failure).OK || len(req.Headers) != 0 {
		t.Fatalf("trace 替代了执行身份或未清理: %s %+v", failure, req.Headers)
	}
}
