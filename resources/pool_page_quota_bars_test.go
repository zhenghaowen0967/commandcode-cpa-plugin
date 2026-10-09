package resources

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPoolPageQuotaBarsStructure(t *testing.T) {
	page := readPoolPage(t)
	for _, required := range []string{
		`class="account-list"`, `class="scope-details"`, `class="group-explanation"`,
		`['five_hour', '5 小时'], ['weekly', '周'], ['month', '月']`,
		`timeZone: 'Asia/Shanghai'`, `subscription_period_end`, `subscription_status`,
		`monthly_credits`, `plan_allowance`, `订阅本期结束`, `续订状态待确认`,
		`不代表实际服务失效`, `role="tooltip"`, `pointerenter`, `pointerleave`,
		`addEventListener('focus'`, `addEventListener('blur'`, `event.key === 'Escape'`,
		`aria-describedby`, `quota-table`, `@media(forced-colors:active)`,
		`var(--primary-solid,var(--primary-color,#438bea))`,
	} {
		if !strings.Contains(page, required) {
			t.Errorf("missing quota visualization contract: %s", required)
		}
	}
	for _, forbidden := range []string{`subscriptionExpiresAt`, `currentPeriodEnd`, `w.cap - q.remaining_credits`} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page must not infer monthly resets or expiry: %s", forbidden)
		}
	}
}

func TestPoolPageQuotaBarsBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; mocked quota bars behavior not run")
	}
	cmd := exec.Command(node, "-e", poolPageDOMHarness+poolPageQuotaBarsHarness)
	cmd.Env = append(cmd.Environ(), "TZ=America/Los_Angeles")
	cmd.Stdin = strings.NewReader(poolPageScript(t))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("quota bars UI regression: %v\n%s", err, output)
	}
}

const poolPageQuotaBarsHarness = `
function card(f, index = 0) { return f.get('accountRows').children[index]; }
function quotaRow(f, name, index = 0) { return card(f, index).querySelectorAll('.quota-row').find(r => r.dataset.window === name); }
function fills(row) { return row.querySelectorAll('.quota-fill'); }
function fillAmount(f, name, index = 0) { return fills(quotaRow(f, name, index))[0]?.style.getPropertyValue('--amount'); }
async function reload(f) { await f.get('reloadButton').fire('click'); await f.flush(); }
async function withAccounts(values) { const f = fixture(); f.setAccounts(values); await f.connect(); return f; }
(async () => {
  const a = account('a'); a.name = '研究账号 A · Key 1';
  const b = account('b'); b.name = '研究账号 A · Key 2';
  const f = await withAccounts([a, b]);
  assert.equal(f.get('accountRows').children.length, 1, 'same group must render one card, not per-key cards');
  assert.equal(card(f).querySelectorAll('h3')[0].textContent, '研究账号 A', 'recognizable account name, never group id');
  const main = visibleText(card(f));
  for (const technical of ['sha256:abcd', 'group_id', 'auth_id', 'real-account-a', '组共享在途', 'headroom', '逐 Key 管理', ' cap ']) {
    assert.ok(!main.includes(technical), 'default main layer must hide technical detail: ' + technical);
    assert.ok(card(f).textContent.includes(technical), 'technical information and management remain available: ' + technical);
  }
  assert.ok(main.includes('5 小时')); assert.ok(main.includes('周')); assert.ok(main.includes('月'));
  assert.equal(fillAmount(f, 'five_hour'), '75%'); assert.equal(fillAmount(f, 'weekly'), '90%'); assert.equal(fillAmount(f, 'month'), '30%');
  assert.ok(quotaRow(f, 'month').textContent.includes('30%'), 'monthly ratio uses month.remaining, not total credits including purchased/free');
  assert.ok(quotaRow(f, 'five_hour').textContent.includes('2030/1/1 08:00:00 北京时间'), 'timestamps must use Shanghai despite test host TZ');
  assert.ok(main.includes('2030/1/16 08:00:00 北京时间'), 'period end is visible separately');
  assert.ok(main.includes('订阅本期结束')); assert.ok(main.includes('续订状态待确认')); assert.ok(main.includes('不代表实际服务失效'));
  assert.ok(quotaRow(f, 'month').textContent.includes('重置时间待确认'), 'period end must not become a monthly reset');
  assert.equal(card(f).querySelectorAll('.quota-row').length, 3, 'one row per display period, never add a monthly scoring window');
  assert.equal(card(f).querySelectorAll('.key-entry').length, 2, 'every key has its own management entry');
  assert.equal(f.get('accountRows').querySelectorAll('[data-account-action]').length, 6, 'per-key edit/refresh/delete retained');
  const tables = card(f).querySelectorAll('.quota-table');
  assert.equal(tables.length, 3, 'table alternative and each original key snapshot available');
  assert.ok(tables[0].textContent.includes('按套餐总额推导')); assert.ok(tables[0].textContent.includes('不参与窗口评分'));

  const hit = quotaRow(f, 'five_hour').querySelectorAll('.bar-hit')[0];
  for (const event of ['pointerenter', 'focus', 'click']) {
    await hit.fire(event);
    assert.equal(f.get('quotaTooltip').hidden, false, event + ' opens accessible tooltip');
    assert.equal(hit.attributes['aria-describedby'], 'quotaTooltip');
    assert.ok(f.get('quotaTooltip').textContent.includes('剩余 75%'));
    assert.ok(f.get('quotaTooltip').textContent.includes('已用：25 credits'));
    assert.ok(f.get('quotaTooltip').textContent.includes('总额：100 credits'));
    assert.ok(f.get('quotaTooltip').textContent.includes('2030/1/1 08:00:00 北京时间'));
    await f.window.fire('keydown', {key: 'Escape'});
    assert.equal(f.get('quotaTooltip').hidden, true, 'Escape dismisses tooltip');
    assert.ok(!('aria-describedby' in hit.attributes));
  }
  await hit.fire('pointerenter'); await hit.fire('pointerleave'); assert.equal(f.get('quotaTooltip').hidden, true);
  await hit.fire('focus'); await hit.fire('blur'); assert.equal(f.get('quotaTooltip').hidden, true);
  await hit.fire('click'); await reload(f); assert.equal(f.get('quotaTooltip').hidden, true, 'polling clears obsolete tooltip');

  let details = card(f).querySelectorAll('.group-details')[0]; details.open = true;
  await reload(f); details = card(f).querySelectorAll('.group-details')[0];
  assert.equal(details.open, true, 'group details remain expanded after polling');
  f.get('accountFilter').value = 'does-not-match'; await f.get('accountFilter').fire('input');
  assert.equal(f.get('accountRows').children.length, 0);
  f.get('accountFilter').value = 'auth-b'; await f.get('accountFilter').fire('input');
  assert.equal(f.get('accountRows').children.length, 1, 'filter searches hidden technical auth_id');
  assert.equal(card(f).querySelectorAll('.key-entry').length, 2, 'filter matches the whole group without hiding another key');
  assert.equal(card(f).querySelectorAll('.group-details')[0].open, true, 'filter round-trip retains detail expansion');
  for (const filter of ['sha256:abcd', 'real-account-a', 'test@example.invalid', 'real-a', 'pro']) {
    f.get('accountFilter').value = filter; await f.get('accountFilter').fire('input');
    assert.equal(f.get('accountRows').children.length, 1, 'hidden field filter: ' + filter);
  }
  card(f).querySelectorAll('.group-details')[0].open = false; await reload(f);
  assert.equal(card(f).querySelectorAll('.group-details')[0].open, false, 'collapsed state is also preserved');
  await f.get('disconnectButton').fire('click'); await f.connect();
  assert.equal(card(f).querySelectorAll('.group-details')[0].open, false, 'disconnect clears retained expansion state');

  const newer = account('newer'); newer.quota.updated_at = new Date(Date.now() - 500).toISOString();
  newer.quota.windows[0] = {...newer.quota.windows[0], used: 20, remaining: 80};
  const older = account('older'); older.quota.updated_at = new Date(Date.now() - 5000).toISOString();
  older.quota.windows[0] = {...older.quota.windows[0], used: 50, remaining: 50};
  const newestError = account('error'); newestError.quota.updated_at = new Date().toISOString(); newestError.quota.error = 'credits response invalid';
  const pick = await withAccounts([older, newestError, newer]);
  assert.equal(fillAmount(pick, 'five_hour'), '80%', 'choose latest valid snapshot, not array order, sum, maximum headroom, or latest error');
  assert.ok(visibleText(card(pick)).includes('同组额度快照有差异'));
  assert.ok(visibleText(card(pick)).includes('credits response invalid'), 'a selected good snapshot must not hide another key error');
  assert.ok(card(pick).querySelectorAll('.key-entry').find(k => k.dataset.account === 'older').textContent.includes('50'));
  const future = account('future'); future.quota.updated_at = '2099-01-01T00:00:00Z';
  future.quota.windows[0] = {...future.quota.windows[0], used: 0, remaining: 100};
  pick.setAccounts([older, future, newer]); await reload(pick);
  assert.equal(fillAmount(pick, 'five_hour'), '80%', 'future timestamps are not valid latest snapshots');
  assert.ok(visibleText(card(pick)).includes('其他凭据的额度未知或数据无效'));

  const monthUnknown = account(); delete monthUnknown.quota.month; monthUnknown.quota.monthly_credits = 14.6;
  monthUnknown.quota.remaining_credits = 99; delete monthUnknown.quota.subscription_period_end; delete monthUnknown.quota.subscription_status;
  const unknown = await withAccounts([monthUnknown]);
  assert.equal(fills(quotaRow(unknown, 'month')).length, 0, 'unknown denominator must have no zero fill');
  assert.ok(quotaRow(unknown, 'month').textContent.includes('月剩余 14.6 credits（总额未知）'));
  assert.ok(!quotaRow(unknown, 'month').textContent.includes('99'), 'total purchased/free credits must never replace unknown monthly balance');
  assert.ok(!quotaRow(unknown, 'month').textContent.includes('0%'));
  assert.ok(visibleText(card(unknown)).includes('订阅本期结束 待确认'));
  const unknownHit = quotaRow(unknown, 'month').querySelectorAll('.bar-hit')[0]; await unknownHit.fire('click');
  assert.ok(unknown.get('quotaTooltip').textContent.includes('剩余：14.6 credits'));
  assert.ok(card(unknown).querySelectorAll('.quota-table')[0].textContent.includes('14.6'));
  delete monthUnknown.quota.monthly_credits; unknown.setAccounts([monthUnknown]); await reload(unknown);
  assert.ok(!quotaRow(unknown, 'month').textContent.includes('月剩余'), 'metadata absent remains unknown');

  for (const bad of [
    {used: 20, cap: 0, remaining: 0},
    {used: 20, cap: null, remaining: 80},
    {used: 20, cap: 100, remaining: null},
    {used: '20', cap: 100, remaining: 80},
    {used: -1, cap: 100, remaining: 101},
    {used: 20, cap: 100, remaining: 50},
    {used: 0, cap: 1e-308, remaining: .1},
  ]) {
    const malformed = account(); malformed.quota.windows[0] = {name: 'five_hour', ...bad};
    const badWindow = await withAccounts([malformed]);
    assert.equal(fills(quotaRow(badWindow, 'five_hour')).length, 0, 'invalid raw metrics may not fabricate a chart: ' + JSON.stringify(bad));
    assert.ok(quotaRow(badWindow, 'five_hour').textContent.includes('待确认'));
  }
  const wrongSource = account(); wrongSource.quota.month.source = 'unknown';
  const badMonth = await withAccounts([wrongSource]); assert.equal(fills(quotaRow(badMonth, 'month')).length, 0);
  assert.ok(quotaRow(badMonth, 'month').textContent.includes('来源待确认'));

  const errorAccount = account(); errorAccount.quota.error = '<img src=x onerror=alert(1)>';
  const upstreamError = await withAccounts([errorAccount]);
  assert.equal(card(upstreamError).querySelectorAll('.quota-fill').length, 0, 'upstream errors cannot draw even when numeric fields are zeroed or retained');
  assert.ok(visibleText(card(upstreamError)).includes('<img src=x onerror=alert(1)>'));
  assert.equal(card(upstreamError).querySelectorAll('img').length, 0, 'labels and errors remain plain text');

  for (const at of [0, '0', null, 'not-a-date', '2030-02-30T00:00:00Z', '2030-01-01T24:00:00Z', '2030-01-01 00:00:00', '0001-01-01T00:00:00Z', '2099-01-01T00:00:00Z']) {
    const invalidTime = account(); invalidTime.quota.updated_at = at;
    const invalidDate = await withAccounts([invalidTime]);
    assert.ok(card(invalidDate).className.includes('stale'), 'invalid/future snapshot is unknown or expired: ' + at);
    assert.equal(card(invalidDate).querySelectorAll('.quota-fill').length, 0, 'invalid/future snapshot timestamp cannot create a current quota chart: ' + at);
  }
  const zeroTimes = account(); zeroTimes.quota.month.reset_at = '0001-01-01T00:00:00Z'; zeroTimes.quota.subscription_period_end = '0001-01-01T00:00:00Z';
  const zeroDates = await withAccounts([zeroTimes]);
  assert.equal(fillAmount(zeroDates, 'month'), '30%', 'unknown reset does not erase valid quota reading');
  assert.ok(quotaRow(zeroDates, 'month').textContent.includes('重置时间待确认'));
  assert.ok(visibleText(card(zeroDates)).includes('订阅本期结束 待确认'));
  const realZero = account(); realZero.quota.month.remaining = 0; realZero.quota.month.used = 70; realZero.quota.monthly_credits = 0;
  const exhaustedMonth = await withAccounts([realZero]); assert.equal(fillAmount(exhaustedMonth, 'month'), '0%', 'real zero with a valid cap is not unknown');
  delete realZero.quota.month; exhaustedMonth.setAccounts([realZero]); await reload(exhaustedMonth);
  assert.equal(fills(quotaRow(exhaustedMonth, 'month')).length, 0); assert.ok(quotaRow(exhaustedMonth, 'month').textContent.includes('月剩余 0 credits'));

  const staleAccount = account(); staleAccount.quota.updated_at = '2001-01-01T00:00:00Z';
  const stale = await withAccounts([staleAccount]);
  assert.ok(card(stale).className.includes('stale')); assert.equal(fillAmount(stale, 'five_hour'), '75%', 'stale snapshot remains historical, not reset to zero');
  assert.ok(visibleText(card(stale)).includes('额度已过期'));
  assert.ok(quotaRow(stale, 'five_hour').textContent.includes('历史记录'));
  await quotaRow(stale, 'five_hour').querySelectorAll('.bar-hit')[0].fire('focus');
  assert.ok(stale.get('quotaTooltip').textContent.includes('历史快照已过期'));

  const overcap = account(); overcap.quota.windows[0] = {name: 'five_hour', used: 100.0001558585, cap: 100, remaining: 0};
  const exceeded = await withAccounts([overcap]);
  assert.equal(fillAmount(exceeded, 'five_hour'), '0%');
  assert.ok(visibleText(card(exceeded)).includes('额度超出总额'));
  assert.ok(card(exceeded).querySelectorAll('.quota-table')[0].textContent.includes('100.0001558585'), 'raw overcap value retains original numeric precision');
  overcap.quota.windows[0] = {name: 'five_hour', used: 0, cap: 100, remaining: 120}; exceeded.setAccounts([overcap]); await reload(exceeded);
  assert.equal(fillAmount(exceeded, 'five_hour'), '100%', 'over-100 fill is safely clipped');
  assert.ok(quotaRow(exceeded, 'five_hour').textContent.includes('120%'), 'raw ratio and warning are never clamped away');
  overcap.quota.windows[0] = {name: 'five_hour', used: 120, cap: 100, remaining: -20}; exceeded.setAccounts([overcap]); await reload(exceeded);
  assert.equal(fillAmount(exceeded, 'five_hour'), '0%'); assert.ok(quotaRow(exceeded, 'five_hour').textContent.includes('-20%'));

  const disabled = account(); disabled.enabled = false; disabled.status = 'disabled';
  const quarantined = account('quarantine'); quarantined.status = 'cleanup_unconfirmed'; quarantined.max_concurrency = 7; quarantined.inflight = 2;
  const warning = await withAccounts([disabled, quarantined]);
  const warningText = visibleText(card(warning));
  assert.ok(warningText.includes('已禁用')); assert.ok(warningText.includes('清理未确认，占位未释放'));
  assert.ok(warningText.includes('cap 不一致')); assert.ok(warningText.includes('组共享在途快照不一致'));
  assert.equal(warning.get('groupRows').children[0].children[2].textContent, '2', 'shared inflight uses known maximum, never sum');
  const unnamed = account(); unnamed.name = ''; const noName = await withAccounts([unnamed]);
  assert.equal(card(noName).querySelectorAll('h3')[0].textContent, '未命名账号', 'unknown name must not silently fall back to technical group id');

  const duplicate = account(); duplicate.quota.windows.push({name: 'five_hour', used: 80, cap: 100, remaining: 20});
  const duplicates = await withAccounts([duplicate]);
  assert.equal(fills(quotaRow(duplicates, 'five_hour')).length, 0, 'duplicate windows do not pick an arbitrary valid-looking number');
  assert.ok(card(duplicates).textContent.includes('原始窗口 five_hour · 已用 80 / cap 100 · 剩余 20'));
  const missing = account(); missing.quota = {};
  const absent = await withAccounts([missing]);
  assert.equal(card(absent).querySelectorAll('.quota-fill').length, 0); assert.ok(visibleText(card(absent)).includes('额度未知'));

  const other = account('c', 'other-real-group'); other.name = '研究账号 C';
  const separate = await withAccounts([a, b, other]);
  assert.equal(separate.get('accountRows').children.length, 2, 'each real group gets exactly one card');
  assert.equal(fillAmount(separate, 'five_hour', 0), '75%'); assert.equal(fillAmount(separate, 'five_hour', 1), '75%');
  process.stdout.write('pool page quota bars regressions passed\n');
})().catch(error => {console.error(error); process.exitCode = 1;});
`
