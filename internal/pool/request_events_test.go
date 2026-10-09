package pool

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestRequestEventsSurviveBackgroundNoise(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	a := addAccount(t, p, "request-events-secret", "owner", 1)
	p.Pick([]string{a.AuthID}, "real-request", "model")
	lease, err := p.Acquire(a.AuthID, "real-request", "model")
	if err != nil {
		t.Fatal(err)
	}
	lease.Record("upstream_cleanup_unconfirmed")
	lease.Settle("upstream_complete")
	before := p.RequestEvents(0)
	if len(before) != 4 {
		t.Fatalf("request events = %d", len(before))
	}
	for i := 0; i < 600; i++ {
		p.AbortRequest("unrelated")
	}
	after := p.RequestEvents(0)
	if len(after) != 4 || after[0].Sequence != before[0].Sequence {
		t.Fatal("noise evicted request events")
	}
	if len(p.Events(0)) != 500 || p.Events(0)[0].Action != "request_aborted" {
		t.Fatal("raw diagnostics changed")
	}
	if len(p.RequestEvents(after[2].Sequence)) != 1 {
		t.Fatal("request cursor wrong")
	}
	if len(p.RequestEvents(^uint64(0))) != 0 {
		t.Fatal("max cursor wrong")
	}
	if _, err := p.Acquire(a.AuthID, "unrelated", "model"); err == nil {
		t.Fatal("terminal protection removed")
	}
}

func TestRequestEventsBoundedAndDeepCopied(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	const secret = "request-events-secret"
	a := addAccount(t, p, secret, "owner", 1)
	for i := 0; i < 600; i++ {
		p.Pick([]string{a.AuthID, "unknown " + secret}, "req "+secret, "model "+secret)
	}
	events := p.RequestEvents(0)
	if len(events) != 500 {
		t.Fatal("request ring not bounded")
	}
	payload, _ := json.Marshal(events)
	if bytes.Contains(payload, []byte(secret)) {
		t.Fatal("request events leaked secret")
	}
	events[0].Reason = "mutated"
	events[0].Candidates[0].Reason = "mutated"
	fresh := p.RequestEvents(0)
	if fresh[0].Reason == "mutated" || fresh[0].Candidates[0].Reason == "mutated" {
		t.Fatal("request event storage mutated")
	}
}

func TestRequestEventsRequireRequestModelAndAllowedAction(t *testing.T) {
	p, _ := testPool(t, time.Minute)
	p.mu.Lock()
	for _, e := range []Event{{RequestID: "r", Action: "pick"}, {Model: "m", Action: "pick"}, {RequestID: "r", Model: "m", Action: "quota_observed"}, {RequestID: "r", Model: "m", Action: "request_aborted"}, {RequestID: "r", Model: "m", Action: "acquire_rejected"}} {
		p.appendLocked(e)
	}
	p.mu.Unlock()
	got := p.RequestEvents(0)
	if len(got) != 1 || got[0].Action != "acquire_rejected" {
		t.Fatalf("unexpected request actions: %+v", got)
	}
}
