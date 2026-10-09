package resources

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func readCodexPage(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile("codex_page.html")
	if err != nil {
		t.Fatal(err)
	}
	return string(page)
}

func codexPageScript(t *testing.T) string {
	t.Helper()
	matches := regexp.MustCompile(`(?s)<script>\s*(.*?)\s*</script>`).FindAllStringSubmatch(readCodexPage(t), -1)
	if len(matches) != 1 {
		t.Fatalf("expected one self-contained script, got %d", len(matches))
	}
	return matches[0][1]
}

func TestCodexPageSecurityAndContract(t *testing.T) {
	page := readCodexPage(t)
	for _, required := range []string{
		`<html lang="zh-CN">`, `Codex 429 临时禁用`, `仅保存在插件内存中`, `到期恢复，重启丢失`,
		`不会清 CPA 内建冷却或 CPAMP 禁用文件`, `无法据此强制恢复流量`,
		`const API_BASE = '/v0/management/plugins/commandcode-pool'`,
		`'codex/bans', 'codex/unban', 'codex/unban-all'`, `const POLL_INTERVAL = 5000`, `const REQUEST_TIMEOUT = 20000`,
		`credentials: 'same-origin'`, `redirect: 'error'`, `cache: 'no-store'`,
		`'Authorization': 'Bearer ' + managementKey`, `response.status === 401 || response.status === 403`, `data.host_cooldown_independent !== true`,
		`data.scope !== 'single_process'`, `data.state !== 'memory_only'`,
		`{auth_id: authId}`, `mutate('codex/unban-all', {},`, `window.confirm`, `single_process`,
		`textContent = ban.auth_id`, `button.dataset.authId = ban.auth_id`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("missing security or management contract: %s", required)
		}
	}
	for _, forbidden := range []string{
		"localStorage", "sessionStorage", "indexedDB", "document.cookie", "innerHTML", "outerHTML",
		"insertAdjacentHTML", "document.write", "postMessage", "window.open", "eval(", "new Function",
		"http://", "https://", "<iframe", "<script src", "@import",
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
	for _, match := range regexp.MustCompile(`\$\('([^']+)'\)`).FindAllStringSubmatch(codexPageScript(t), -1) {
		if !seen[match[1]] {
			t.Errorf("script references missing DOM id: %s", match[1])
		}
	}
}

func TestCodexPageHostThemeAndEmbed(t *testing.T) {
	page := readCodexPage(t)
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
	if !strings.Contains(readCodexPage(t), "Codex 429 临时禁用") {
		t.Error("Codex page content missing")
	}
	if !strings.Contains(CodexPage, "Codex 429 临时禁用") {
		t.Error("CodexPage embed does not contain the Codex page")
	}
}

func TestCodexPageJavaScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; JavaScript syntax check not run")
	}
	cmd := exec.Command(node, "--check")
	cmd.Stdin = strings.NewReader(codexPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("JavaScript syntax check: %v\n%s", err, output)
	}
}

// This runs the page script against a mock same-origin management API; it does not start a server.
func TestCodexPageManagementBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; mocked management flow not run")
	}
	cmd := exec.Command(node, "-e", codexPageBehaviorHarness)
	cmd.Stdin = strings.NewReader(codexPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Codex management UI regression: %v\n%s", err, output)
	}
}

const codexPageBehaviorHarness = `
'use strict';
const assert = require('node:assert/strict');
const vm = require('node:vm');
const script = require('node:fs').readFileSync(0, 'utf8');
class Element {
  constructor(tag = 'div') {
    this.tagName = tag; this.children = []; this.listeners = {}; this.dataset = {};
    this.value = ''; this.hidden = false; this.disabled = false; this.className = ''; this._text = '';
    this.attributes = {}; this.classList = {add() {}, remove() {}};
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(' '); }
  append(...nodes) { this.children.push(...nodes); }
  appendChild(node) { this.children.push(node); return node; }
  replaceChildren(...nodes) { this._text = ''; this.children = [...nodes]; }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  async fire(type, extra = {}) { for (const listener of this.listeners[type] || []) await listener({preventDefault() {}, ...extra}); }
  reset() { for (const input of this.resetInputs || []) input.value = ''; }
  querySelectorAll(selector) {
    const result = [];
    function walk(node) {
      for (const child of node.children) {
        if ((selector === '[data-action]' && child.dataset.action) || (selector === '.countdown' && child.className === 'countdown')) result.push(child);
        walk(child);
      }
    }
    walk(this); return result;
  }
}
function fixture(custom) {
  const nodes = new Map();
  const get = id => { if (!nodes.has(id)) nodes.set(id, new Element()); return nodes.get(id); };
  get('connectionForm').resetInputs = [get('managementKey')];
  const calls = []; const timers = new Map(); const timerHistory = []; let timerID = 0; const pageListeners = {};
  let confirms = 0; let responseBans = [{auth_id: '<img src=x onerror=alert(1)>', window: '5h', banned_at: '2026-10-08T10:00:00Z', reset_at: '2026-10-08T11:00:00Z'}];
  const window = {confirm: () => { confirms++; return true; }, addEventListener(type, listener) { (pageListeners[type] ||= []).push(listener); }};
  const document = {getElementById: get, createElement: tag => new Element(tag), body: new Element('body')};
  async function fetch(url, options) {
    const call = {url, options, body: options.body ? JSON.parse(options.body) : null}; calls.push(call);
    const result = custom ? await custom(call) : undefined;
    if (result && result.promise) return result.promise;
    const data = result || (url.endsWith('/codex/bans') ? {bans: responseBans, scope: 'single_process', state: 'memory_only', host_cooldown_independent: true} : {cleared: true, cleared_count: 1, bans: []});
    const status = data.httpStatus || 200;
    return {ok: status >= 200 && status < 300, status, json: async () => data};
  }
  const context = {
    document, window, fetch, AbortController, console,
    setTimeout: (fn, delay) => { const id = ++timerID; timers.set(id, {fn, delay, kind: 'timeout'}); timerHistory.push(delay); return id; },
    clearTimeout: id => timers.delete(id),
    setInterval: (fn, delay) => { const id = ++timerID; timers.set(id, {fn, delay, kind: 'interval'}); return id; },
    clearInterval: id => timers.delete(id),
  };
  vm.runInNewContext(script, context);
  async function flush() { for (let i = 0; i < 14; i++) await Promise.resolve(); await new Promise(r => setImmediate(r)); }
  async function connect() { get('managementKey').value = 'fake-management-key'; await get('connectionForm').fire('submit'); await flush(); }
  return {get, calls, timers, timerHistory, pageListeners, flush, connect, confirms, get confirmsCount() { return confirms; }, setBans(value) { responseBans = value; }};
}
(async () => {
  const f = fixture(); await f.connect();
  assert.equal(f.get('managementKey').value, '', 'management key input must clear immediately');
  assert.equal(f.calls.length, 1, 'connect fetches the Codex ban list');
  const first = f.calls[0];
  assert.equal(first.url, '/v0/management/plugins/commandcode-pool/codex/bans');
  assert.equal(first.options.method, 'GET');
  assert.equal(first.options.headers.Authorization, 'Bearer fake-management-key');
  assert.equal(first.options.redirect, 'error'); assert.equal(first.options.credentials, 'same-origin'); assert.equal(first.options.cache, 'no-store');
  assert.equal(f.get('banRows').children.length, 1);
  const row = f.get('banRows').children[0];
  assert.equal(row.children[0].textContent, '<img src=x onerror=alert(1)>', 'auth_id rendered as inert text');
  assert.equal(row.children[5].children[0].dataset.authId, '<img src=x onerror=alert(1)>', 'auth_id bound as data, not executable handler code');
  assert.ok(!row.children.some(node => node.tagName === 'img'));
  assert.equal(f.get('scopeState').textContent, '作用范围：single_process · memory_only · 主机冷却独立');
  assert.equal([...f.timers.values()].filter(timer => timer.delay === 5000).length, 1, 'one 5-second poll is scheduled');
  assert.ok(f.timerHistory.includes(20000), 'request timeout is registered');
  const button = row.children[5].children[0];
  await f.get('banRows').fire('click', {target: {closest: selector => selector === '[data-action="unban"]' ? button : null}}); await f.flush();
  const unban = f.calls.at(-1);
  assert.equal(unban.url, '/v0/management/plugins/commandcode-pool/codex/unban');
  assert.equal(unban.options.method, 'POST'); assert.deepEqual(unban.body, {auth_id: '<img src=x onerror=alert(1)>'});
  assert.equal(f.get('banRows').children.length, 0);

  const concurrent = fixture();
  concurrent.setBans([
    {auth_id: 'auth-one', window: '5h', banned_at: '2026-10-08T10:00:00Z', reset_at: '2026-10-08T11:00:00Z'},
    {auth_id: 'auth-two', window: '7d', banned_at: '2026-10-08T10:00:00Z', reset_at: '2026-10-08T11:00:00Z'},
  ]);
  await concurrent.connect();
  const actionButtons = concurrent.get('banRows').children.map(item => item.children[5].children[0]);
  await concurrent.get('banRows').fire('click', {target: {closest: selector => selector === '[data-action="unban"]' ? actionButtons[0] : null}});
  await concurrent.get('banRows').fire('click', {target: {closest: selector => selector === '[data-action="unban"]' ? actionButtons[1] : null}});
  await concurrent.flush();
  assert.equal(concurrent.calls.filter(call => call.url.endsWith('/codex/unban')).length, 1, 'mutations cannot overlap');

  const all = fixture(); await all.connect();
  await all.get('unbanAllButton').fire('click'); await all.flush();
  assert.equal(all.confirmsCount, 2, 'unban all requires two confirmations');
  const clear = all.calls.at(-1);
  assert.equal(clear.url, '/v0/management/plugins/commandcode-pool/codex/unban-all');
  assert.equal(clear.options.method, 'POST'); assert.deepEqual(clear.body, {});
  assert.ok(all.get('message').textContent.includes('已清除 1 条'));

  const denied = fixture(call => call.url.endsWith('/codex/bans') ? {httpStatus: 403} : undefined);
  await denied.connect();
  assert.equal(denied.timers.size, 0, '401/403 stops poll and request timers');
  assert.equal(denied.get('banRows').children.length, 0);
  assert.equal(denied.get('managementKey').value, '');
  assert.equal(denied.get('managementKey').disabled, false);
  assert.ok(denied.get('syncError').textContent.includes('HTTP 403'));

  let resolvePending;
  const pending = fixture(() => ({promise: new Promise(resolve => {resolvePending = resolve;})}));
  pending.get('managementKey').value = 'pending-key'; await pending.get('connectionForm').fire('submit');
  await pending.get('disconnectButton').fire('click');
  resolvePending({ok: true, status: 200, json: async () => ({bans: [{auth_id: 'late', window: 'x', banned_at: '2026-10-08T10:00:00Z', reset_at: '2026-10-08T11:00:00Z'}], scope: 'single_process', state: 'memory_only', host_cooldown_independent: true})});
  await pending.flush();
  assert.equal(pending.get('banRows').children.length, 0, 'late response cannot restore disconnected data');
  assert.equal(pending.timers.size, 0); assert.ok(pending.calls[0].options.signal.aborted);

  let resolvePagehide;
  const pagehide = fixture(() => ({promise: new Promise(resolve => {resolvePagehide = resolve;})}));
  pagehide.get('managementKey').value = 'pagehide-key'; await pagehide.get('connectionForm').fire('submit');
  for (const listener of pagehide.pageListeners.pagehide || []) listener();
  assert.equal(pagehide.get('banRows').children.length, 0);
  assert.equal(pagehide.get('managementKey').value, '');
  assert.ok(pagehide.calls[0].options.signal.aborted, 'pagehide cancels outstanding fetch');
  resolvePagehide({ok: true, status: 200, json: async () => ({bans: [{auth_id: 'late-pagehide', window: 'x', banned_at: '2026-10-08T10:00:00Z', reset_at: '2026-10-08T11:00:00Z'}], scope: 'single_process', state: 'memory_only', host_cooldown_independent: true})});
  await pagehide.flush();
  assert.equal(pagehide.timers.size, 0, 'pagehide clears polling and countdown timers');
  assert.equal(pagehide.get('banRows').children.length, 0, 'pagehide response cannot restore cleared data');
  assert.ok(!pagehide.get('banRows').textContent.includes('pagehide-key'));
  process.stdout.write('Codex page mock-DOM management regressions passed\n');
})().catch(error => {console.error(error); process.exitCode = 1;});
`
