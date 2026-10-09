package resources

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func readPoolPage(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile("pool_page.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(page)
}

func poolPageScript(t *testing.T) string {
	t.Helper()
	matches := regexp.MustCompile(`(?s)<script>\s*(.*?)\s*</script>`).FindAllStringSubmatch(readPoolPage(t), -1)
	if len(matches) != 1 {
		t.Fatalf("expected one self-contained script, got %d", len(matches))
	}
	return matches[0][1]
}

func TestPoolPageSecurityAndContract(t *testing.T) {
	page := readPoolPage(t)
	for _, required := range []string{
		`<html lang="zh-CN">`, `type="password"`, `textContent`, `replaceChildren`,
		`const API_BASE = '/v0/management/plugins/commandcode-pool'`,
		`credentials: 'same-origin'`, `redirect: 'error'`, `cache: 'no-store'`,
		`'Authorization': 'Bearer ' + managementKey`, `new AbortController()`,
		`const POLL_INTERVAL = 5000`, `const EVENT_LIMIT = 500`,
		`accounts/import`, `accounts/delete`, `quota/refresh`, `next_cursor`,
		`quota_max_age_seconds`, `single_process`, `home_supported`,
		`remaining_credits`, `headroom`, `reset_at`, `candidate.eligible`,
		`candidate.auth_id`, `event.attempt_id`, `window.confirm`, `overflow-x:auto`,
		`组共享在途`, `同组只计一次`, `API 未提供单 Key 在途`,
		`管理 Key`, `部分成功`, `共享并发 cap`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("missing security or management contract: %s", required)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "indexedDB", "document.cookie",
		"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write",
		"console.", "navigator.sendBeacon", "postMessage", "window.open",
		"localStorage", "http://", "https://", "<iframe", "<script src", "@import",
		"group.inflight +=", "'此 Key '",
	} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page contains forbidden external or secret-handling construct: %s", forbidden)
		}
	}
	if regexp.MustCompile(`(?i)\s(?:src|href)\s*=`).MatchString(page) {
		t.Error("page must not load external assets or link to another origin")
	}
	ids := regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(page, -1)
	seen := make(map[string]bool)
	for _, match := range ids {
		if seen[match[1]] {
			t.Errorf("duplicate DOM id: %s", match[1])
		}
		seen[match[1]] = true
	}
	for _, match := range regexp.MustCompile(`\$\('([^']+)'\)`).FindAllStringSubmatch(poolPageScript(t), -1) {
		if !seen[match[1]] {
			t.Errorf("script references missing DOM id: %s", match[1])
		}
	}
}

func TestPoolPageHostTheme(t *testing.T) {
	page := readPoolPage(t)
	for _, required := range []string{
		`--bg:var(--app-bg,#101722)`, `--surface:var(--app-surface,#162131)`,
		`--raised:var(--app-surface-muted,#1c2b40)`, `--text:var(--text-primary,#edf3fc)`,
		`:root:not([data-cpamp-plugin-host='true']){color-scheme:dark}`,
		`color:var(--primary-contrast,#fff)`, `tbody tr:hover{background:var(--raised)}`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("missing host theme or standalone fallback: %s", required)
		}
	}
	for _, forbidden := range []string{`background:#132033`, `background:#19273a`, `background:#111d2d`, `:root{color-scheme:dark`} {
		if strings.Contains(page, forbidden) {
			t.Errorf("fixed dark style conflicts with host light theme: %s", forbidden)
		}
	}
}

func TestPoolPageJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; JavaScript syntax check not run")
	}
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(poolPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript syntax check: %v\n%s", err, output)
	}
}

// 此测试执行实际页面脚本与隔离管理 API，不访问网络；浏览器布局验证另行执行。
func TestPoolPageManagementBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; mocked management flow not run")
	}
	cmd := exec.Command(node, "-e", poolPageBehaviorHarness)
	cmd.Stdin = strings.NewReader(poolPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("management UI regression: %v\n%s", err, output)
	}
}

const poolPageBehaviorHarness = `
'use strict';
const assert = require('node:assert/strict');
const vm = require('node:vm');
const script = require('node:fs').readFileSync(0, 'utf8');
class Element {
  constructor(tag = 'div') {
    this.tagName = tag; this.children = []; this.listeners = {}; this.dataset = {};
    this.value = ''; this.checked = true; this.hidden = false; this.disabled = false; this.open = false;
    this._text = ''; this.className = ''; this.attributes = {};
    this.classList = {add: () => {}, remove: () => {}};
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(' '); }
  append(...nodes) { this.children.push(...nodes); }
  replaceChildren(...nodes) { this._text = ''; this.children = [...nodes]; }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  async fire(type) { for (const listener of this.listeners[type] || []) await listener({preventDefault() {}}); }
  showModal() { this.open = true; }
  close() { this.open = false; for (const listener of this.listeners.close || []) listener({}); }
  reset() { for (const input of this.resetInputs || []) { input.value = ''; input.checked = true; } }
  querySelectorAll(selector) {
    const result = [];
    function walk(node) {
      for (const child of node.children) {
        if ((selector === 'details[open]' && child.tagName === 'details' && child.open) ||
            (selector === '[data-account-action]' && child.dataset.accountAction)) result.push(child);
        walk(child);
      }
    }
    walk(this); return result;
  }
}
const defaultStatus = {plugin_version: 'test-version', scope: 'single_process', quota_max_age_seconds: 120, default_limit: 4, home_supported: false};
function account(id = 'a', group = 'real-account-a') {
  return {id, auth_id: 'auth-' + id, name: '账号 ' + id, group_id: group, max_concurrency: 4, enabled: true, inflight: 1, key_fingerprint: 'sha256:abcd', status: 'ready', quota: {remaining_credits: 42, headroom: .75, updated_at: new Date().toISOString(), email: 'test@example.invalid', plan: 'pro', identity: 'real-a', windows: [{name: '小时窗口', used: 25, cap: 100, remaining: 75, reset_at: new Date().toISOString()}]}};
}
function fixture(custom) {
  const nodes = new Map();
  const get = id => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); };
  get('connectionForm').resetInputs = [get('managementKey')];
  get('accountForm').resetInputs = ['accountName', 'accountGroup', 'accountLimit', 'accountKey', 'accountEnabled'].map(get);
  get('importForm').resetInputs = [get('importJSON')];
  const calls = []; const timers = new Map(); let timerID = 0; let responseAccounts = [account(), account('b')];
  const window = {confirm: () => true, addEventListener: () => {}};
  const document = {getElementById: get, createElement: tag => new Element(tag), body: new Element('body'), querySelectorAll: selector => Array.from(nodes.values()).flatMap(n => n.querySelectorAll(selector))};
  async function fetch(url, options) {
    const call = {url, options, body: options.body ? JSON.parse(options.body) : null}; calls.push(call);
    let result = custom && await custom(call);
    if (result && result.promise) return result.promise;
    if (!result) {
      if (url.endsWith('/accounts') && options.method === 'GET') result = {accounts: responseAccounts};
      else if (url.includes('/events?')) result = {events: [], next_cursor: '0'};
      else if (url.endsWith('/status')) result = {status: defaultStatus};
      else if (url.endsWith('/accounts/import')) result = {accounts: call.body.accounts.map((a, i) => ({...account('import-' + i), name: a.name, group_id: a.group_id}))};
      else if (url.endsWith('/accounts') && options.method === 'POST') result = {account: account('saved')};
      else result = {};
    }
    const status = result.httpStatus || 200;
    return {ok: status >= 200 && status < 300, status, text: async () => result.raw || JSON.stringify(result)};
  }
  const context = {document, window, fetch, AbortController, console, setTimeout: (fn, delay) => { const id = ++timerID; timers.set(id, {fn, delay}); return id; }, clearTimeout: id => timers.delete(id)};
  vm.runInNewContext(script, context);
  async function flush() { for (let i = 0; i < 14; i++) await Promise.resolve(); await new Promise(r => setImmediate(r)); }
  async function connect() { get('managementKey').value = 'fake-management-key'; await get('connectionForm').fire('submit'); await flush(); }
  function pageText() { return Array.from(nodes.values()).map(n => n.textContent).join(' '); }
  return {get, calls, timers, flush, connect, pageText, setAccounts(value) {responseAccounts = value;}};
}
function countPosts(f) { return f.calls.filter(c => c.options.method === 'POST').length; }
(async () => {
  const f = fixture(); await f.connect();
  assert.equal(f.get('managementKey').value, '', 'management input must clear immediately');
  assert.equal(f.calls.length, 3, 'connection loads accounts, events and status');
  for (const call of f.calls) {
    assert.ok(call.url.startsWith('/v0/management/plugins/commandcode-pool/'));
    assert.equal(call.options.headers.Authorization, 'Bearer fake-management-key');
    assert.equal(call.options.redirect, 'error'); assert.equal(call.options.credentials, 'same-origin'); assert.equal(call.options.cache, 'no-store');
  }
  assert.ok(f.pageText().includes('单进程'));
  assert.ok(f.pageText().includes('Home 聚合：未支持'));
  assert.ok(f.get('groupRows').textContent.includes('2 / 2'));
  assert.ok(f.get('groupRows').textContent.includes('同组 cap 一致'));
  assert.equal(f.get('groupRows').children[0].children[2].textContent, '1', 'same shared inflight returned on two keys must count once');
  for (const row of f.get('accountRows').children) {
    assert.ok(row.children[4].textContent.includes('组共享在途 1 / 4'));
    assert.ok(row.children[4].textContent.includes('API 未提供单 Key 在途'));
  }
  assert.equal(f.timers.size, 1, 'only polling timer remains');
  assert.equal([...f.timers.values()][0].delay, 5000);
  await f.get('pollButton').fire('click'); assert.equal(f.timers.size, 0);
  assert.ok(f.get('connectionLabel').textContent.includes('暂停'));
  await f.get('pollButton').fire('click'); await f.flush(); assert.equal(f.timers.size, 1);
  const edit = f.get('accountRows').querySelectorAll('[data-account-action]')[0]; await edit.fire('click');
  assert.equal(f.get('accountKey').value, '', 'edit never echoes key');
  assert.equal(f.get('accountKey').required, false);
  f.get('accountLimit').value = '6'; await f.get('accountLimit').fire('input');
  assert.ok(f.get('groupCapHint').textContent.includes('原子更新整组 cap'));
  await f.get('accountForm').fire('submit'); await f.flush();
  let save = f.calls.find(c => c.url.endsWith('/accounts') && c.options.method === 'POST');
  assert.equal(save.body.id, 'a'); assert.equal(save.body.max_concurrency, 6); assert.ok(!('api_key' in save.body));
  assert.equal(f.get('accountDialog').open, false);
  await f.get('addButton').fire('click');
  f.get('accountName').value = 'new key'; f.get('accountGroup').value = 'real-account-a'; f.get('accountLimit').value = '9'; f.get('accountKey').value = 'fake-account-secret';
  const previousPosts = countPosts(f); await f.get('accountForm').fire('submit');
  assert.equal(countPosts(f), previousPosts, 'new key must not change existing group cap');
  assert.equal(f.get('accountKey').value, ''); assert.ok(f.get('accountFormError').textContent.includes('现有共享 cap'));
  f.get('accountLimit').value = '4'; f.get('accountKey').value = 'fake-account-secret';
  await f.get('accountForm').fire('submit'); await f.flush();
  save = f.calls.filter(c => c.url.endsWith('/accounts') && c.options.method === 'POST').at(-1);
  assert.equal(save.body.api_key, 'fake-account-secret'); assert.equal(f.get('accountKey').value, '');
  assert.ok(!f.pageText().includes('fake-account-secret'));
  await f.get('importButton').fire('click');
  f.get('importJSON').value = JSON.stringify([{name: 'key1', group_id: 'real-new', api_key: 'fake-import-1', max_concurrency: 2}, {name: 'key2', group_id: 'real-new', api_key: 'fake-import-2', max_concurrency: 3}]);
  const beforeImport = countPosts(f); await f.get('importForm').fire('submit');
  assert.equal(countPosts(f), beforeImport); assert.equal(f.get('importJSON').value, ''); assert.ok(f.get('importFormError').textContent.includes('必须一致'));
  f.get('importJSON').value = '{bad JSON secret'; await f.get('importForm').fire('submit');
  assert.equal(f.get('importJSON').value, ''); assert.ok(!f.pageText().includes('bad JSON secret'));
  f.get('importJSON').value = JSON.stringify({accounts: [{name: 'key1', group_id: 'real-new', api_key: 'fake-import-1', max_concurrency: 2}]});
  await f.get('importForm').fire('submit'); await f.flush();
  assert.ok(f.get('importResult').textContent.includes('成功 1 / 提交 1')); assert.equal(f.get('importDialog').open, false); assert.ok(!f.pageText().includes('fake-import-1'));
  const deleteButton = f.get('accountRows').querySelectorAll('[data-account-action]')[2]; await deleteButton.fire('click'); await f.flush();
  assert.ok(f.calls.some(c => c.url.endsWith('/accounts/delete') && c.body.id === 'a'));
  await f.get('refreshAllButton').fire('click'); await f.flush();
  assert.ok(f.calls.some(c => c.url.endsWith('/quota/refresh') && Object.keys(c.body).length === 0));
  await f.get('disconnectButton').fire('click'); assert.equal(f.timers.size, 0);
  assert.equal(f.get('accountRows').children.length, 0); assert.equal(f.get('groupRows').children.length, 0); assert.equal(f.get('eventRows').children.length, 0); assert.equal(f.get('importResult').textContent, '');
  assert.equal(f.get('managementKey').value, ''); assert.equal(f.get('managementKey').disabled, false); assert.ok(!f.pageText().includes('fake-management-key'));

  const capDecrease = fixture(); await capDecrease.connect();
  await capDecrease.get('accountRows').querySelectorAll('[data-account-action]')[0].fire('click');
  capDecrease.get('accountLimit').value = '1'; await capDecrease.get('accountForm').fire('submit'); await capDecrease.flush();
  assert.ok(capDecrease.calls.some(c => c.options.method === 'POST' && c.body.max_concurrency === 1), 'cap equal to shared inflight must not be rejected due to duplicate keys');

  const mismatchedA = account('snapshot-a'); const mismatchedB = account('snapshot-b'); mismatchedB.inflight = 2;
  const mismatched = fixture(call => call.url.endsWith('/accounts') && call.options.method === 'GET' ? {accounts: [mismatchedA, mismatchedB]} : undefined);
  await mismatched.connect();
  assert.equal(mismatched.get('groupRows').children[0].children[2].textContent, '2', 'different group snapshots use the maximum, never the sum');
  assert.ok(mismatched.get('groupRows').textContent.includes('组共享在途快照不一致'));

  const partial = fixture(call => call.url.endsWith('/accounts/import') ? {accounts: [account('one')], error: 'fake-import-secret must never echo'} : undefined);
  await partial.connect(); await partial.get('importButton').fire('click');
  partial.get('importJSON').value = JSON.stringify([{name: 'one', group_id: 'new', api_key: 'fake-import-secret', max_concurrency: 2}, {name: 'two', group_id: 'new', api_key: 'fake-import-secret-2', max_concurrency: 2}]);
  await partial.get('importForm').fire('submit'); await partial.flush();
  assert.ok(partial.get('importResult').textContent.includes('部分成功：成功 1 / 提交 2'));
  assert.ok(partial.get('importResult').textContent.includes('ID one')); assert.ok(!partial.pageText().includes('fake-import-secret'));

  const old = account(); old.quota.updated_at = '2001-01-01T00:00:00Z'; old.quota.error = '<img src=x onerror=alert(1)>';
  const stale = fixture(call => call.url.endsWith('/accounts') && call.options.method === 'GET' ? {accounts: [old]} : undefined);
  await stale.connect(); assert.equal(stale.get('quotaWarning').hidden, false); assert.ok(stale.get('quotaWarning').textContent.includes('1 个 Key'));
  assert.ok(stale.get('accountRows').textContent.includes('<img src=x onerror=alert(1)>'));
  assert.ok(!stale.get('accountRows').children.some(n => n.tagName === 'img'), 'untrusted text must not become markup');

  let eventPass = 0;
  const eventFixture = fixture(call => {
    if (!call.url.includes('/events?')) return undefined;
    eventPass++;
    const candidates = [{account_id: 'a', auth_id: 'auth-a', group_id: 'real-account-a', headroom: .75, remaining_credits: 42, inflight: 2, limit: 4, eligible: true, reason: 'highest_headroom'}, {account_id: 'b', auth_id: 'auth-b', group_id: 'real-account-a', headroom: .5, remaining_credits: 10, inflight: 2, limit: 4, eligible: false, reason: 'quota_stale'}];
    return {events: Array.from({length: 505}, (_, i) => ({sequence: i + 1, at: new Date().toISOString(), action: 'pick', request_id: 'request-' + i, attempt_id: 'attempt-1', model: 'test-model', account_id: 'a', group_id: 'real-account-a', reason: 'actual_decision_reason', candidates})), next_cursor: 505};
  });
  await eventFixture.connect(); assert.equal(eventFixture.get('eventRows').children.length, 500);
  assert.ok(eventFixture.get('eventRows').textContent.includes('highest_headroom')); assert.ok(eventFixture.get('eventRows').textContent.includes('quota_stale')); assert.ok(eventFixture.get('eventRows').textContent.includes('attempt-1'));
  eventFixture.get('eventRows').children[0].children.at(-1).open = true;
  await eventFixture.get('reloadButton').fire('click'); await eventFixture.flush();
  assert.equal(eventFixture.get('eventRows').children.length, 500, 'duplicate events must not accumulate');
  assert.equal(eventFixture.get('eventRows').children[0].children.at(-1).open, true, 'open candidate details remain open after polling');
  await eventFixture.get('clearEventsButton').fire('click'); assert.equal(eventFixture.get('eventRows').children.length, 0); assert.ok(eventFixture.get('eventSummary').textContent.includes('游标 505'));

  let largePass = 0;
  const large = fixture(call => call.url.includes('/events?') ? (++largePass === 1 ? {raw: '{"events":[{"sequence":18446744073709551614,"action":"selected"}],"next_cursor":18446744073709551614}'} : {events: [], next_cursor: '18446744073709551614'}) : undefined);
  await large.connect(); assert.ok(large.get('eventSummary').textContent.includes('18446744073709551614'));
  await large.get('reloadButton').fire('click'); await large.flush(); assert.ok(large.calls.some(c => c.url.endsWith('scope=requests&after=18446744073709551614')));

  function ev(sequence, request_id, action, reason, attempt_id = 'one', account_id = 'a') {
    return {sequence, request_id, action, reason, attempt_id, account_id, model: 'test-model', at: new Date().toISOString()};
  }
  const requestData = [
    ev(1, 'success', 'pick', 'selected'), ev(2, 'success', 'acquire', 'acquired'), ev(3, 'success', 'settle', 'upstream_complete'),
    ev(4, 'rejected', 'pick', 'no_eligible_account', '', ''),
    ev(5, 'canceled', 'pick', 'selected'), ev(6, 'canceled', 'acquire', 'acquired'), ev(7, 'canceled', 'settle', 'upstream_canceled'),
    ev(8, 'retry', 'pick', 'selected'), ev(9, 'retry', 'acquire', 'acquired'), ev(10, 'retry', 'settle', 'upstream_network_failure'), ev(11, 'retry', 'pick', 'selected', 'two', 'b'), ev(12, 'retry', 'acquire', 'acquired', 'two', 'b'), ev(13, 'retry', 'settle', 'upstream_complete', 'two', 'b'),
    ev(14, 'quarantine', 'pick', 'selected'), ev(15, 'quarantine', 'acquire', 'acquired'), ev(16, 'quarantine', 'quarantined', 'upstream_cleanup_unconfirmed'),
    ev(17, 'incomplete', 'settle', 'upstream_complete'),
    ev(18, 'running', 'pick', 'selected'), ev(19, 'running', 'acquire', 'acquired'),
    ev(20, 'noise', 'request_aborted', 'request_finished'), ev(21, 'noise', 'quota_observed', 'eligible'),
    ev(22, 'cap-race', 'pick', 'selected'), ev(23, 'cap-race', 'acquire_rejected', 'concurrency_limit'),
    ev(24, 'alias-done', 'pick', 'selected'), ev(25, 'alias-done', 'acquire', 'acquired'), ev(26, 'alias-done', 'settle', 'done'),
    ev(27, 'alias-cancelled', 'pick', 'selected'), ev(28, 'alias-cancelled', 'acquire', 'acquired'), ev(29, 'alias-cancelled', 'settle', 'cancelled')
  ];
  const grouped = fixture(call => call.url.includes('/events?') ? {events: [...requestData].reverse(), next_cursor: 29} : undefined);
  await grouped.connect(); assert.equal(grouped.get('eventRows').children.length, 10, 'one row per real request, ordered input and noise handled');
  const requestRow = id => grouped.get('eventRows').children.find(n => n.dataset.request === id);
  assert.equal(requestRow('success').children[0].textContent.includes('成功'), true);
  assert.ok(requestRow('rejected').children[0].textContent.includes('未执行'));
  assert.ok(requestRow('rejected').children[1].textContent.includes('未执行'));
  assert.ok(requestRow('canceled').children[0].textContent.includes('已取消'));
  assert.ok(requestRow('retry').children[0].textContent.includes('成功'));
  assert.ok(requestRow('retry').textContent.includes('2 次实际尝试'));
  assert.ok(requestRow('quarantine').children[0].textContent.includes('占位未释放'));
  assert.ok(requestRow('incomplete').children[0].textContent.includes('历史信息不完整'));
  assert.ok(requestRow('running').children[0].textContent.includes('执行中'));
  assert.ok(requestRow('cap-race').children[0].textContent.includes('未执行'));
  assert.ok(requestRow('cap-race').children[2].textContent.includes('准入拒绝：账号共享并发已满'));
  assert.ok(requestRow('alias-done').children[0].textContent.includes('成功'), 'settled done must read as success');
  assert.ok(!requestRow('alias-done').children[0].textContent.includes('失败'));
  assert.ok(requestRow('alias-cancelled').children[0].textContent.includes('已取消'), 'settled cancelled must read as canceled');
  assert.ok(!requestRow('success').children[1].textContent.includes('request_id'), 'technical IDs must be collapsed');
  assert.ok(grouped.calls.some(c => c.url.includes('scope=requests&after=0')));
  await grouped.get('disconnectButton').fire('click'); assert.equal(grouped.get('eventRows').children.length, 0);
  const clipped = fixture(call => call.url.includes('/events?') ? {events: Array.from({length: 100}, (_, i) => ev(i + 1, 'one-request', 'pick', 'selected')), next_cursor: 100} : undefined);
  await clipped.connect(); assert.equal(clipped.get('eventRows').children.length, 1);
  assert.ok(clipped.get('eventRows').textContent.includes('50 条过程记录'));
  assert.ok(clipped.get('eventRows').children[0].children[0].textContent.includes('历史信息不完整'));

  const forbidden = fixture(call => call.url.endsWith('/status') ? {httpStatus: 401} : undefined);
  await forbidden.connect(); assert.equal(forbidden.timers.size, 0); assert.equal(forbidden.get('accountRows').children.length, 0); assert.equal(forbidden.get('syncError').hidden, false); assert.ok(forbidden.get('syncError').textContent.includes('HTTP 401')); assert.equal(forbidden.get('managementKey').disabled, false);
  const badResponse = fixture(call => call.url.endsWith('/accounts') ? {httpStatus: 500} : undefined);
  await badResponse.connect(); assert.ok(badResponse.get('syncError').textContent.includes('HTTP 500')); assert.equal(badResponse.timers.size, 1);

  let resolveAccounts;
  const pending = fixture(call => call.url.endsWith('/accounts') ? {promise: new Promise(resolve => {resolveAccounts = resolve;})} : undefined);
  pending.get('managementKey').value = 'fake-pending-key'; await pending.get('connectionForm').fire('submit');
  await pending.get('disconnectButton').fire('click');
  resolveAccounts({ok: true, status: 200, text: async () => JSON.stringify({accounts: [account('late')]})}); await pending.flush();
  assert.equal(pending.get('accountRows').children.length, 0, 'late response must not restore disconnected data'); assert.equal(pending.timers.size, 0); assert.ok(pending.calls.every(c => c.options.signal.aborted));
  process.stdout.write('pool page mock-DOM management regressions passed\n');
})().catch(error => {console.error(error); process.exitCode = 1;});
`
