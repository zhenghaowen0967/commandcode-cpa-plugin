"""账号池事件重建与整批归属证明：从脱敏 request 事件与采样重建 lease。

输入是插件请求过程视图的脱敏事件（sequence/action/reason/account_ordinal/候选评分）
以及全池采样（inflight/cap/status）。本模块只做纯计算，不接触生产、不联网。

两条互相独立的判断：
1. 集合级观测（observe_pool）：只看事件流本身，重建新 lease 的峰值、自然释放与满载重选，
   明确标注“这些是池内观测到的事件，不等于逐条归属于测试”。
2. 整批归属（prove_coverage）：在集合级观测之外，再要求“成功请求数 == 新 Acquire 数”
   以及账号分布守恒，从而证明整批新 lease 属于本轮，而不是逐条 UUID 配对。

两种模式：
- 'host'：每条请求带唯一宿主 request_id；事件按 host id 过滤（observe_pool mode='test'）。
- 'legacy'：旧 0.1.7 集合证据没有 host id，改用 session_id + 最终唯一成功 usage trace +
  exact_session 关联；此时事件无法按 host 过滤，必须用完整 all-events 集合（mode='all'）。

无论哪种模式都**不**把 usage/客户端 trace 当作 host id，也**不**回填/伪造 host UUID。
"""
import math
import re
import statistics
from collections import Counter

from evidence import compare_identity, parse_timestamp

SETTLE_COMPLETE = 'upstream_complete'
LOCAL_CAP_REASON = 'concurrency_limit'
DEFAULT_GROUP_CAP = 10
DEFAULT_RING_CAPACITY = 500

MODE_HOST = 'host'
MODE_LEGACY = 'legacy'
POOL_SCOPE_TEST = 'host_id_matched_requests'
POOL_SCOPE_ALL = 'all_observed_pool_requests_not_attributed_to_tests'


def _token(event):
    """一个 lease 的身份是 (宿主请求ID, 尝试ID)：重试会换 attempt 而不换 request。"""
    return (event.get('request_id'), event.get('attempt_id'))


def _ordinal(value):
    """把账号序号归一为 int；JSON 里 1 与 "1" 混用时不能造成假守恒失败。"""
    if isinstance(value, bool) or value is None:
        return None
    if isinstance(value, int):
        return value
    if isinstance(value, str) and value.strip().lstrip('-').isdigit():
        return int(value)
    return None


def _iter(events):
    return sorted(events, key=lambda e: e['sequence'])


def observe_pool(events, mode='all', host_ids=None, group_cap=DEFAULT_GROUP_CAP):
    """重建事件流里的新 lease 集合，返回峰值、自然释放、满载重选与证据缺口。

    报出的峰值是“观测到的在途”，语义上属于整个池窗口内的新 lease，而不是逐条测试归属；
    调用方必须结合 prove_coverage 才能把整批算作本轮测试。
    """
    if mode not in ('all', 'test'):
        raise ValueError('unknown_mode')

    active, seen, completed = {}, set(), set()
    peaks, total_peak = {}, 0
    errors, nonnatural = [], []
    cap_rejections, busy_switches = [], []
    last_selected, rejected, pending_switches, confirmed = {}, {}, {}, set()

    host_ids = set(host_ids or ())
    match_all = mode == 'all'

    def is_target(event):
        return match_all or event.get('request_id') in host_ids

    for event in _iter(events):
        if not is_target(event):
            continue
        token = _token(event)
        group = _ordinal(event.get('account_ordinal'))
        action = event['action']
        reason = event.get('reason')

        if action == 'acquire':
            # 账号序号必须是正整数：缺失/0/负数一律视为未映射，不参与峰值与守恒。
            if token in seen or group is None or group <= 0:
                errors.append('duplicate_or_unmapped_acquire')
                continue
            seen.add(token)
            active[token] = group
            inflight = sum(g == group for g in active.values())
            peaks[group] = max(peaks.get(group, 0), inflight)
            total_peak = max(total_peak, len(active))
            for switch in pending_switches.pop(event['request_id'], []):
                key = (event['request_id'], switch['sequence'],
                       switch['full_account_ordinal'], group)
                if switch['selected_account_ordinal'] == group and key not in confirmed:
                    busy_switches.append({**switch, 'confirmed_acquire_sequence': event['sequence']})
                    confirmed.add(key)
            if inflight > group_cap:
                errors.append('observed_group_cap_exceeded')
        elif action == 'settle':
            if token not in active:
                errors.append('settle_without_acquire')
                continue
            active.pop(token)
            completed.add(token)
            if reason != SETTLE_COMPLETE:
                nonnatural.append({'request_id': event.get('request_id'), 'reason': reason})
        elif action == 'pick' and reason == 'selected':
            candidates = event.get('candidates', [])
            chosen = next((c for c in candidates if _ordinal(c.get('account_ordinal')) == group), None)
            for candidate in candidates:
                if (chosen and _ordinal(candidate.get('account_ordinal')) != group
                        and candidate.get('reason') == LOCAL_CAP_REASON
                        and candidate.get('inflight') == candidate.get('limit') == group_cap
                        and (candidate.get('headroom'), candidate.get('remaining_credits'))
                        > (chosen.get('headroom'), chosen.get('remaining_credits'))):
                    pending_switches.setdefault(event['request_id'], []).append({
                        'request_id': event['request_id'], 'sequence': event['sequence'],
                        'method': 'full_candidate_pick',
                        'full_account_ordinal': _ordinal(candidate.get('account_ordinal')),
                        'selected_account_ordinal': group})
            previous = rejected.get(event['request_id'])
            prior = last_selected.get(event['request_id'])
            if (chosen and previous and prior and previous['account_ordinal'] != group
                    and prior.get('account_ordinal') == previous['account_ordinal']
                    and previous['full_inflight'] == group_cap
                    and (prior.get('headroom'), prior.get('remaining_credits'))
                    > (chosen.get('headroom'), chosen.get('remaining_credits'))):
                pending_switches.setdefault(event['request_id'], []).append({
                    'request_id': event['request_id'], 'sequence': event['sequence'],
                    'method': 'same_host_cap_rejection_then_pick',
                    'rejection_sequence': previous['sequence'], 'full_inflight': group_cap,
                    'full_account_ordinal': previous['account_ordinal'],
                    'selected_account_ordinal': group})
            if chosen:
                last_selected[event['request_id']] = chosen
        elif action == 'acquire_rejected' and reason == LOCAL_CAP_REASON:
            cap_rejections.append({
                'request_id': event.get('request_id'), 'sequence': event['sequence'],
                'account_ordinal': group,
                'full_inflight': sum(g == group for g in active.values())})
            rejected[event['request_id']] = cap_rejections[-1]
        elif action in ('acquire_rejected', 'quarantined'):
            errors.append('observed_' + action)

    return {
        'scope': POOL_SCOPE_ALL if match_all else POOL_SCOPE_TEST,
        'acquire_count': len(seen), 'settle_count': len(completed),
        'remaining_new_owners': len(active), 'group_peaks': peaks, 'total_peak': total_peak,
        'natural_settle_count': len(completed) - len(nonnatural),
        'nonnatural_settles': nonnatural, 'busy_switch_events': busy_switches,
        'local_cap_rejections': cap_rejections, 'errors': sorted(set(errors)),
        'host_request_id_attribution_proven': False}


def response_statistics(cases):
    """统计完整响应耗时与成功数；只针对“完整读取到结束”的响应，不是首token延迟。"""
    done = [case for case in cases if case.get('elapsed_seconds') is not None]
    values = sorted(case['elapsed_seconds'] for case in done)

    def percentile(ratio):
        if not values:
            return None
        return values[max(0, math.ceil(len(values) * ratio) - 1)]

    return {
        'count': len(cases),
        'http_200_count': sum(case.get('http') == 200 for case in cases),
        'complete_response_count': sum(case.get('attempt_status') == 'verified' for case in cases),
        'failure_count': sum(bool(case.get('failure')) or case.get('http') not in (None, 200)
                             for case in cases),
        'complete_response_seconds': {
            'min': values[0] if values else None,
            'median': statistics.median(values) if values else None,
            'p95': percentile(0.95),
            'max': values[-1] if values else None},
    }


def _legacy_success_and_attempts(rows):
    """旧 0.1.7 集合证据：每个 session 必须恰好一个最终成功 usage，其余是明确的本地cap拒绝。

    返回 (final_success_rows, rejection_rows)；任何无法解释的失败都抛错，绝不吞掉。
    verified 只由“最终唯一成功尝试”确定，而不是由调用方传入的布尔。
    """
    finals, rejections = [], []
    for row in rows:
        usage = row.get('cpa_usage') or []
        ok = [u for u in usage if not u.get('failed') and u.get('fail_status_code') == 200]
        bad = [u for u in usage if u.get('failed')]
        if len(ok) != 1 or len(ok) + len(bad) != len(usage):
            raise ValueError('legacy_usage_final_success_not_unique')
        if any(u.get('request_id') != ok[0].get('request_id') for u in usage):
            raise ValueError('legacy_usage_trace_changed_within_session')
        if any(u.get('fail_status_code') != 429 or u.get('latency_ms') != 0
               or not u.get('confirmed_local_cap_rejection') for u in bad):
            raise ValueError('legacy_unexplained_usage_failure_attempt')
        finals.append(row)
        rejections.extend(bad)
    return finals, rejections


OWNER_ACTIONS = ('pick', 'acquire', 'settle', 'acquire_rejected', 'quarantined')
# owner 的 lease 生命周期事件：这些必须携带可信 trace，才能逐条连接。
LEASE_ACTIONS = ('pick', 'acquire', 'settle', 'quarantined')


def _derive_hosts_from_events(trace_id, events):
    """用事件里记录的 client trace 反查宿主 request_id 集合。

    这是逐条连接的真实来源：事件的 host↔trace 关系，而不是调用方自报。
    允许同一 trace 对应多个 host（typed 合同允许 parent trace 复用），此时返回全部 host，
    只拒绝“声称唯一配对”，不把合法共享 trace 当成整体失败。
    """
    if not trace_id:
        return []
    return sorted({e['request_id'] for e in events
                   if e.get('trace_id') == trace_id and e.get('request_id')})


def _consistent_event_trace(events, host, trace_id):
    """host 的**全部** lease 生命周期事件（pick/acquire/settle/quarantined）trace 必须
    都等于该 trace：不能靠任一 pick 提供 UUID 就让无 trace 的 acquire/settle 假 join。

    本地 cap 拒绝（acquire_rejected）不是 lease 生命周期事件、合法地不带 trace；
    但它若带了 trace，也必须与 owner trace 一致。任一带外来 trace 都判不一致。
    """
    owner_lease = [e for e in events if e.get('request_id') == host
                   and e.get('action') in LEASE_ACTIONS]
    if not owner_lease:
        return False
    if any(e.get('trace_id') != trace_id for e in owner_lease):
        return False
    return all(e.get('trace_id') in (None, trace_id)
               for e in events if e.get('request_id') == host)


def _valid_trace(value):
    """标准 UUID（含版本/变体位）才可视为可信 client trace。"""
    if not isinstance(value, str):
        return False
    return bool(re.match(
        r'\A[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}'
        r'-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}\Z', value))


def _snapshot_accounts(samples):
    """校验采样账号形状：非空、各快照组集合相同、inflight 非负且不超过 cap。

    返回错误列表（不再返回形状，避免调用方误把它并进错误集合）。
    """
    errors = []
    if not samples:
        return errors
    shapes = []
    for snap in samples:
        accounts = snap.get('accounts')
        if not isinstance(accounts, list) or not accounts:
            errors.append('snapshot_accounts_empty')
            continue
        shape = sorted(_ordinal(a.get('account_ordinal')) for a in accounts)
        if any(o is None or o <= 0 for o in shape):
            errors.append('snapshot_account_ordinal_invalid')
        if len(set(shape)) != len(shape):
            errors.append('snapshot_account_ordinal_duplicate')
        shapes.append(tuple(shape))
        for account in accounts:
            cap = account.get('cap', DEFAULT_GROUP_CAP)
            inflight = account.get('inflight')
            if not isinstance(inflight, int) or isinstance(inflight, bool) or inflight < 0:
                errors.append('snapshot_inflight_invalid')
            elif inflight > cap:
                errors.append('snapshot_cap_contract_failed')
            if cap != DEFAULT_GROUP_CAP:
                errors.append('snapshot_account_cap_not_shared')
    if shapes and len(set(shapes)) != 1:
        errors.append('snapshot_account_shape_changed')
    return errors


def derive_mode(host_rows, events):
    """判定证据模式：任一请求带 host id 走 'host'，否则走 'legacy'。"""
    if any(isinstance(row.get('host_request_id'), str) and row['host_request_id']
           for row in host_rows):
        return MODE_HOST
    return MODE_LEGACY


def prove_coverage(host_rows, observations, prior_cursor, pool, mode=None,
                   identity_expectation=None):
    """用“成功请求必有独立 lease”的合同和完整事件等量，证明整批新 lease 属于本轮。

    - host_rows：每个被测请求一条。host 模式须有唯一宿主 request_id；旧证据可只带
      session_id + cpa_usage + server_execution。
    - observations：{'events': [...], 'samples': [...], 'event_completeness_readback': {...}}。
    - prior_cursor：本轮采样开始前的 request 事件游标。
    - pool：observe_pool 的结果；host 模式用 mode='test'，legacy 模式必须用 mode='all'。
    - identity_expectation：可选的 {'baseline': {...}}，与 identity 逐字段核对。

    绝不把 usage trace 当 host id，也不回填/伪造 host UUID；缺 host id 时只做集合级证明，
    并明确报 per_request_host_trace_join_present=False。
    """
    events = observations.get('events', [])
    samples = observations.get('samples', [])
    errors = []
    if mode is None:
        mode = derive_mode(host_rows, events)
    if mode not in (MODE_HOST, MODE_LEGACY):
        raise ValueError('unknown_mode')

    def flag(condition, name):
        if not condition:
            errors.append(name)

    if mode == MODE_HOST:
        for row in host_rows:
            host_id = row.get('host_request_id')
            flag(isinstance(host_id, str) and bool(host_id), 'host_id_required')
            flag(row.get('attempt_status') == 'verified', 'request_not_verified')
            flag(row.get('http') == 200, 'request_not_http_200')
            flag(row.get('host_request_id_is_client_fabricable') is not True,
                 'host_id_must_not_be_client_fabricable')
        host_ids = [row.get('host_request_id') for row in host_rows]
        flag(len(set(host_ids)) == len(host_ids) and None not in host_ids, 'host_ids_not_unique')
        flag(set(host_ids) == {e['request_id'] for e in events},
             'event_host_ids_do_not_match_cases')
    else:
        try:
            _legacy_success_and_attempts(host_rows)
        except ValueError as exc:
            errors.append(str(exc))
        for row in host_rows:
            if not row.get('session_id'):
                flag(False, 'legacy_session_id_required')
            if row.get('association') != 'exact_session':
                flag(False, 'legacy_association_not_exact_session')
            if not row.get('cpa_usage'):
                flag(False, 'legacy_usage_required')
        if pool['errors'] == [] and any(e.get('request_id') not in {None} for e in events):
            # 集合证据的等价要求：事件整体就等于成功集合，pool 覆盖也必须来自全体事件。
            flag(pool['acquire_count'] == len(host_rows),
                 'legacy_all_events_not_equal_to_success_set')

    # 逐条可信用 client trace 时，用**事件**的 host↔trace 关系逐 host 核对。
    # 允许 1 trace→N host（typed 合同），只拒绝“无事件证据”与“事件不一致”。
    trace_links = []
    per_request_trace_join = False
    all_traces_present = all(row.get('client_trace_id') for row in host_rows)
    if all_traces_present:
        case_host_ids = {row.get('host_request_id') for row in host_rows} - {None}
        joined_all = True
        for row in host_rows:
            trace = row['client_trace_id']
            if not _valid_trace(trace):
                errors.append('client_trace_id_must_be_standard_uuid')
                joined_all = False
                continue
            if trace in case_host_ids:
                errors.append('trace_id_must_not_pose_as_host_id')
                joined_all = False
            hosts = _derive_hosts_from_events(trace, events)
            hint = row.get('host_request_id')
            # 事件必须至少提供证据，且（若调用方给了提示）提示必须落在事件 host 集合内。
            if not hosts:
                errors.append('event_trace_has_no_host_evidence')
                joined_all = False
                continue
            if hint is not None and hint not in hosts:
                errors.append('event_trace_host_mismatch')
                joined_all = False
                continue
            checked = [h for h in hosts if _consistent_event_trace(events, h, trace)]
            if hint is not None:
                consistent = hint in checked
            else:
                consistent = bool(checked)
            if not consistent:
                errors.append('event_owner_events_trace_inconsistent')
                joined_all = False
            trace_links.append({
                'trace_id': trace, 'host_request_ids': hosts,
                'host_request_id_count': len(hosts),
                'unique_pairing': len(hosts) == 1,
                'verified_host_request_ids': checked,
                'event_evidence': 'verified' if consistent else 'inconsistent',
                'join_source': 'event_trace',
                'host_request_id_is_client_fabricable': bool(
                    row.get('host_request_id_is_client_fabricable')),
            })
        per_request_trace_join = joined_all and len(trace_links) == len(host_rows)
    elif any(row.get('client_trace_id') for row in host_rows):
        if mode == MODE_HOST:
            errors.append('per_request_trace_incomplete')
        per_request_trace_join = False

    if identity_expectation:
        baseline = identity_expectation.get('baseline')
        for row in host_rows:
            mismatched = compare_identity(row.get('identity', {}), baseline or {})
            if mismatched:
                flag(False, 'identity_baseline_mismatch:' + ','.join(sorted(mismatched)))

    flag(len(events) > 0, 'missing_events')
    flag(len(samples) > 0, 'missing_boundary_samples')
    errors.extend(_snapshot_accounts(samples))
    if samples:
        accounts = samples[0].get('accounts') or []
        flag(bool(accounts) and all(a.get('inflight') == 0 for a in accounts),
             'boundary_pool_not_empty_at_start')
        accounts = samples[-1].get('accounts') or []
        flag(bool(accounts) and all(a.get('inflight') == 0 for a in accounts),
             'boundary_pool_not_empty_at_end')
    if events and samples:
        try:
            flag(parse_timestamp(samples[0]['at'])
                 < min(parse_timestamp(r['started_at_utc']) for r in host_rows),
                 'start_sample_not_before_first_post')
            flag(parse_timestamp(samples[-1]['at'])
                 > max(parse_timestamp(r['finished_at_utc']) for r in host_rows),
                 'end_sample_not_after_last_post')
        except (KeyError, ValueError):
            flag(False, 'boundary_timestamps_unusable')

    sequences = [e['sequence'] for e in events]
    span = max(sequences, default=prior_cursor) - prior_cursor
    flag(bool(sequences) and min(sequences) > prior_cursor, 'event_cursor_not_advanced')
    flag(span < DEFAULT_RING_CAPACITY, 'event_ring_may_have_wrapped')
    flag(len(set(sequences)) == len(sequences), 'duplicate_event_sequence')

    completeness = observations.get('event_completeness_readback', {})
    flag(completeness.get('actual_buffer_retained_entire_period') is True,
         'runtime_event_buffer_completeness_not_confirmed')
    flag(completeness.get('period_events_equal_exactly') is True,
         'period_events_do_not_equal_saved_events')
    flag(completeness.get('initial_cursor') == prior_cursor, 'completeness_cursor_mismatch')
    flag(completeness.get('last_test_event_sequence') == max(sequences, default=None),
         'completeness_last_sequence_mismatch')
    flag(completeness.get('period_request_events') == len(events),
         'completeness_period_event_count_mismatch')
    # 缓存必须仍保留测试前更早的事件：最老保留序号 <= 起始游标，证明整段没被覆盖。
    oldest = completeness.get('oldest_retained_request_sequence')
    flag(isinstance(oldest, int) and not isinstance(oldest, bool) and oldest <= prior_cursor,
         'runtime_event_buffer_retention_boundary_failed')

    acquires = [e for e in events if e['action'] == 'acquire']
    acquire_requests = {e['request_id'] for e in acquires}
    flag(len(acquires) == len(host_rows), 'new_lease_set_not_equal_to_success_set_size')
    flag(len(acquire_requests) == len(host_rows), 'acquire_request_ids_not_unique')

    usage_groups = Counter(_ordinal(row.get('server_execution', {}).get('account_ordinal'))
                           for row in host_rows)
    event_groups = Counter(_ordinal(e.get('account_ordinal')) for e in acquires)
    flag(None not in usage_groups and None not in event_groups and usage_groups == event_groups,
         'account_conservation_not_met')

    # pool 结果必须与证据模式匹配，避免用部分窗口冒充整批。
    if mode == MODE_HOST:
        flag(pool.get('scope') == POOL_SCOPE_TEST, 'pool_scope_mismatch')
    else:
        flag(pool.get('scope') == POOL_SCOPE_ALL, 'pool_scope_mismatch')
    flag(pool.get('errors', []) == [], 'pool_analysis_had_errors')
    flag(pool.get('remaining_new_owners', 1) == 0, 'not_all_new_owners_released')
    flag(pool.get('settle_count', -1) == len(host_rows), 'settle_count_not_equal_success_set')
    flag(pool.get('nonnatural_settles', [None]) == [], 'nonnatural_settle_present')

    return {
        'mode': mode,
        'method': 'complete_new_lease_set_cardinality_and_account_conservation',
        'source_contract': '每个实际成功的CommandCode执行必须获得独立lease，客户端不缓存响应',
        'successful_request_count': len(host_rows),
        'distinct_client_trace_count': len({row.get('client_trace_id') for row in host_rows
                                            if row.get('client_trace_id')}),
        'new_acquire_count': len(acquires),
        'distinct_host_request_count': len(acquire_requests),
        'usage_account_counts': {str(k): v for k, v in usage_groups.items()},
        'acquire_account_counts': {str(k): v for k, v in event_groups.items()},
        'observed_peak': pool.get('total_peak'), 'observed_group_peaks': pool.get('group_peaks'),
        'busy_switch_count': len(pool.get('busy_switch_events', [])),
        'global_sequence_span': span, 'event_ring_capacity': DEFAULT_RING_CAPACITY,
        'runtime_event_buffer_complete': bool(
            completeness.get('actual_buffer_retained_entire_period')),
        'per_request_host_trace_join_present': bool(per_request_trace_join),
        'verified_host_request_count': len({h for link in trace_links
                                            for h in link['verified_host_request_ids']}),
        'trace_links': trace_links,
        'whole_test_lease_set_proven': not errors,
        'errors': sorted(set(errors)),
    }
