package resources

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPoolPageTraceDetailsCompatibilityAndSafeText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; trace UI regression not run")
	}
	cmd := exec.Command(node, "-e", poolPageTraceHarness)
	cmd.Stdin = strings.NewReader(poolPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("追踪详情回归: %v\n%s", err, output)
	}
}

const poolPageTraceHarness = poolPageDOMHarness + `
(async () => {
  const trace = '018f0000-0000-7000-8000-000000000001';
  function event(sequence, request_id, action, trace_id) {
    return {sequence, request_id, trace_id, action, reason: action === 'settle' ? 'upstream_complete' : 'selected', attempt_id: 'attempt-' + request_id, account_id: 'a', model: 'synthetic-model', at: new Date().toISOString()};
  }
  const data = [
    event(1, 'host-traced', 'pick', trace), event(2, 'host-traced', 'acquire', trace), event(3, 'host-traced', 'settle', trace),
    event(4, 'host-legacy', 'pick'), event(5, 'host-legacy', 'acquire'), event(6, 'host-legacy', 'settle'),
    event(7, 'host-conflict', 'pick', trace), event(8, 'host-conflict', 'acquire', '018f0000-0000-7000-8000-000000000002'),
    event(9, 'host-text', 'pick', '<img src=x onerror=alert(1)>')
  ];
  const f = fixture(call => call.url.includes('/events?') ? {events: data, next_cursor: '9'} : undefined);
  await f.connect();
  assert.equal(f.get('eventRows').children.length, 4, 'trace must not merge or split host request rows');
  const row = id => f.get('eventRows').children.find(n => n.dataset.request === id);
  assert.ok(row('host-traced').textContent.includes('宿主 request_id host-traced'));
  assert.ok(row('host-traced').textContent.includes('入站 TraceID ' + trace));
  assert.ok(!visibleText(row('host-traced')).includes(trace), 'technical trace remains collapsed');
  assert.ok(row('host-legacy').textContent.includes('入站 TraceID 未提供'));
  assert.ok(row('host-legacy').children[0].textContent.includes('成功'), 'old events still render normally');
  assert.ok(row('host-conflict').textContent.includes('关联信息不一致'));
  assert.ok(row('host-text').textContent.includes('<img src=x onerror=alert(1)>'));
  assert.equal(row('host-text').querySelectorAll('img').length, 0, 'trace is text, not markup');
  row('host-traced').children.at(-1).open = true;
  await f.get('reloadButton').fire('click'); await f.flush();
  assert.equal(row('host-traced').children.at(-1).open, true, 'trace details remain open during polling');
  process.stdout.write('trace UI regressions passed\n');
})().catch(error => {console.error(error); process.exitCode = 1;});
`
