package pool

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testTraceID = "018f0000-0000-7000-8000-000000000001"

func TestNormalizeTraceID(t *testing.T) {
	for _, id := range []string{testTraceID, "12345678-1234-4ABC-8DEF-123456789ABC"} {
		if NormalizeTraceID(id) != id {
			t.Fatalf("合法 UUID 未保留: %q", id)
		}
	}
	for _, id := range []string{"", "00000001", "host-request", " " + testTraceID, testTraceID + "\r\n", "00000000-0000-0000-0000-000000000000", "018f0000-0000-7000-0000-000000000001", strings.Repeat("x", 4096)} {
		if NormalizeTraceID(id) != "" {
			t.Fatalf("异常 trace 未丢弃: %q", id)
		}
	}
}

func TestTraceFollowsLeaseWithoutChangingAdmission(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "synthetic-trace-key", "owner", 1)
	p.PickWithTrace([]string{a.AuthID}, "host-one", "model", testTraceID)
	if findView(t, p, a.ID).Inflight != 0 {
		t.Fatal("选号不应预占并发")
	}
	lease, err := p.AcquireWithTrace(a.AuthID, "host-one", "model", testTraceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Settle("upstream_complete") })
	if _, err := p.AcquireWithTrace(a.AuthID, "host-two", "model", testTraceID); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("trace 改变了 cap 保护: %v", err)
	}
	lease.Record("upstream_cleanup_unconfirmed")
	lease.Cancel()
	if findView(t, p, a.ID).Inflight != 1 {
		t.Fatal("Cancel 不能代替 owner Settle")
	}
	lease.Settle("upstream_complete")
	lease.Settle("upstream_complete")
	if findView(t, p, a.ID).Inflight != 0 {
		t.Fatal("自然结算没有释放占位")
	}
	events := p.RequestEvents(0)
	want := []string{"pick", "acquire", "acquire_rejected", "quarantined", "settle"}
	if len(events) != len(want) {
		t.Fatalf("事件数不符: %+v", events)
	}
	for i, event := range events {
		if event.Action != want[i] || event.TraceID != testTraceID {
			t.Fatalf("trace 生命周期不完整: %+v", event)
		}
		if event.RequestID == event.TraceID {
			t.Fatal("trace 被误用作 host RequestID")
		}
	}
}

func TestSharedTraceDoesNotMergeOwners(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "synthetic-shared-trace", "owner", 2)
	one, err := p.AcquireWithTrace(a.AuthID, "host-one", "model", testTraceID)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Settle("upstream_complete")
	two, err := p.AcquireWithTrace(a.AuthID, "host-two", "model", testTraceID)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Settle("upstream_complete")
	if one.AttemptID == two.AttemptID || findView(t, p, a.ID).Inflight != 2 {
		t.Fatal("相同 trace 合并了独立 owner")
	}
	p.AbortRequestWithTrace("host-one", testTraceID)
	select {
	case <-two.Context.Done():
		t.Fatal("同 trace 的另一个 host 被误取消")
	default:
	}
	one.Settle("upstream_complete")
	if findView(t, p, a.ID).Inflight != 1 {
		t.Fatal("一个 owner 结算误释放了另一个占位")
	}
}

func TestLegacyTraceOmittedAndMalformedTraceDoesNotBlock(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "synthetic-legacy-trace", "owner", 1)
	p.Pick([]string{a.AuthID}, "host-legacy", "model")
	lease, err := p.Acquire(a.AuthID, "host-legacy", "model")
	if err != nil {
		t.Fatal(err)
	}
	lease.Settle("upstream_complete")
	malformed, err := p.AcquireWithTrace(a.AuthID, "host-malformed", "model", "arbitrary secret text")
	if err != nil {
		t.Fatal(err)
	}
	malformed.Settle("upstream_complete")
	blob, err := json.Marshal(p.RequestEvents(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), "trace_id") || strings.Contains(string(blob), "arbitrary secret text") {
		t.Fatalf("旧/异常 trace 不应增加字段或泄露文本: %s", blob)
	}
}

func TestTraceRedactedForCurrentAndLaterKnownSecret(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "synthetic-later-secret", "owner", 1)
	p.PickWithTrace([]string{a.AuthID}, "host-trace", "model", testTraceID)
	addAccount(t, p, testTraceID, "other-owner", 1)
	for _, events := range [][]Event{p.Events(0), p.RequestEvents(0)} {
		blob, _ := json.Marshal(events)
		if strings.Contains(string(blob), testTraceID) {
			t.Fatal("新增已知秘密后历史 trace 未重新脱敏")
		}
	}
	p.PickWithTrace([]string{a.AuthID}, "host-known-secret", "model", testTraceID)
	for _, event := range p.RequestEvents(0) {
		if event.TraceID != "" {
			t.Fatal("已知秘密被写入 trace")
		}
	}
}
