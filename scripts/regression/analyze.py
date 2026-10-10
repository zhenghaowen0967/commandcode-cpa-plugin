"""离线分析既有JSON证据：重建lease集合、证明整批归属、连接trace与host请求ID。

只读明确给出的证据文件，不访问生产、不联网、不启动服务、不做任何真实模型POST。
输出用 O_EXCL 独占创建：目标已存在即拒绝覆盖，原证据保持不动。

证据协商：analyze 需要三份可自包含复用的输入——
- 请求记录（host_rows）：每个被测请求的宿主ID、可信trace、边界时间与账号序号；
- 观测（observations）：脱敏的 request 事件流与全池采样；
- 事件完整性回读（可内嵌在观测里，也可单列）：证明环形缓存没有覆盖/漏采该区间。

缺少任一份时按“证据不足”失败，绝不用默认值或近似区间伪造通过。
"""
import argparse
import datetime as dt
import json
import sys
from pathlib import Path

from evidence import (UUID_RE, atomic_write_new, canonical_sha256, compare_identity,
                      file_sha256, fingerprint_identity, load_json)
from pool_analysis import MODE_HOST, MODE_LEGACY, derive_mode, observe_pool, prove_coverage, response_statistics

REQUIRED_HINT_FIELDS = ('attempt_status', 'http', 'started_at_utc', 'finished_at_utc',
                        'server_execution')
OPTIONAL_CASE_FIELDS = ('client_trace_id', 'host_request_id')

DEFAULT_ANALYSIS_CONTRACT = {
    'expected_peak': None,          # 未给出则不声称任何峰值容量
    'require_busy_switch': False,   # 未要求则不声称满载重选
    'max_posts': None,              # 未给出则不设请求数上限
    'max_tokens': None,             # 未给出则不设
}


def _now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def _validate_cases(cases):
    """逐条检查请求记录的横切字段；缺字段直接报错，不静默跳过。

    集合证据（旧 0.1.7）可以不提供 `host_request_id`：此时走 legacy 模式，用
    session_id + cpa_usage + exact_session 做集合级等量与守恒证明，绝不回填/伪造 host UUID。
    `client_trace_id` 若给出必须是标准 UUID。
    """
    if not isinstance(cases, list) or not cases:
        raise ValueError('cases_must_be_nonempty_list')
    for index, row in enumerate(cases):
        if not isinstance(row, dict):
            raise ValueError('case_not_object:%s' % index)
        missing = [field for field in REQUIRED_HINT_FIELDS if field not in row]
        if missing:
            raise ValueError('case_missing_fields:%s:%s' % (index, ','.join(missing)))
        trace = row.get('client_trace_id')
        if trace is not None and (not isinstance(trace, str) or not UUID_RE.match(trace)):
            raise ValueError('client_trace_id_must_be_standard_uuid:%s' % index)
        host = row.get('host_request_id')
        if host is not None and (not isinstance(host, str) or not host):
            raise ValueError('host_request_id_must_be_nonempty_string:%s' % index)
    return cases


def connect_trace(cases, trace_id, events=None):
    """把可信 trace_id 连接到 host request_id。

    有 events 时以**事件**里的 host↔trace 关系为准（逐 host 核对 owner 事件 trace 一致），
    允许 1 trace→N host（typed 合同允许 parent trace 复用），此时 `unique_pairing=false`，
    返回全部 `host_request_ids`，不做强行配对。
    没有 events 时**不提供证明能力**：只回 `joined=false`、`join_source='unverified_hint'`，
    明确这是未核验的提示，绝不给人可信错觉。
    """
    if not isinstance(trace_id, str) or not UUID_RE.match(trace_id):
        raise ValueError('trace_id_must_be_standard_uuid')
    hits = [i for i, row in enumerate(cases) if row.get('client_trace_id') == trace_id]
    if not hits:
        raise ValueError('trace_id_must_match_at_least_one_request')
    fabricable = any(bool(cases[i].get('host_request_id_is_client_fabricable')) for i in hits)
    base = {'trace_id': trace_id, 'row_index': hits[0],
            'host_request_id_is_client_fabricable': fabricable}
    if events is None:
        return {**base, 'host_request_ids': [], 'unique_pairing': False, 'joined': False,
                'join_source': 'unverified_hint',
                'verified_host_request_ids': []}
    from pool_analysis import _consistent_event_trace, _derive_hosts_from_events
    hosts = _derive_hosts_from_events(trace_id, events)
    hints = {cases[i].get('host_request_id') for i in hits} - {None}
    if hints and not hints <= set(hosts):
        raise ValueError('event_trace_host_mismatch')
    verified = [h for h in hosts if _consistent_event_trace(events, h, trace_id)]
    joined = bool(verified) and (not hints or hints <= set(verified))
    return {**base, 'host_request_ids': hosts, 'host_request_id_count': len(hosts),
            'unique_pairing': len(hosts) == 1, 'joined': joined,
            'join_source': 'event_trace', 'verified_host_request_ids': verified,
            'event_evidence': 'verified' if joined else 'inconsistent_or_missing'}


def _attach_baseline(cases):
    """返回 (首条身份指纹, 与首条不一致的case下标列表)。

    不能只看首条：任一 case 的身份漂移都必须被报出。
    """
    first, mismatched = None, []
    for index, row in enumerate(cases):
        if not row.get('identity'):
            continue
        fingerprint = fingerprint_identity(row['identity'])
        if first is None:
            first = fingerprint
        elif fingerprint != first:
            mismatched.append(index)
    return first, mismatched


def _all_within(limit, value):
    """未给上限（None）时视为不设限；给出时必须 value <= limit。"""
    if limit is None:
        return True
    return isinstance(value, int) and not isinstance(value, bool) and value <= limit


class AnalysisContract:
    """分析合同：显式声明才声称容量/预算；未给门则不声明。

    不块全量：`expected_peak` 未给时不会声称任何峰值容量；`require_busy_switch` 默认 false。
    这样“17 个集合证明”不会被误报成“峰值 11 的并发容量验收”。
    """

    def __init__(self, expected_peak=None, require_busy_switch=False,
                 max_posts=None, max_tokens=None):
        for name, value in (('expected_peak', expected_peak), ('max_posts', max_posts),
                            ('max_tokens', max_tokens)):
            if value is not None and (not isinstance(value, int) or isinstance(value, bool)
                                      or value < 0):
                raise ValueError('%s_must_be_nonnegative_integer' % name)
        self.expected_peak = expected_peak
        self.require_busy_switch = bool(require_busy_switch)
        self.max_posts = max_posts
        self.max_tokens = max_tokens


def _measured_attempts(pool):
    """事件中的成功 Acquire 与本地准入拒绝之和，不代表客户端 POST 次数。"""
    return pool['acquire_count'] + len(pool['local_cap_rejections'])


def _evaluate_contract(coverage, statistics, contract, baseline_mismatches, budget):
    """按合同判定容量验收；未声明的门不判通过也不判失败，明确列为 unsupported。

    显式声明的门（expected_peak/require_busy_switch）、预算失败和身份漂移都必须参与判定；
    未声明 `expected_peak` 时 collection 通过也不等于容量通过。
    """
    reasons = []
    checks = {}
    observed_peak = coverage.get('observed_peak')
    if contract.expected_peak is None:
        checks['peak'] = 'not_required'
    else:
        checks['peak'] = 'met' if observed_peak is not None and observed_peak >= contract.expected_peak \
            else 'unmet'
        if checks['peak'] == 'unmet':
            reasons.append('observed_peak_below_expected:%s<%s'
                           % (observed_peak, contract.expected_peak))
    if contract.require_busy_switch:
        busy = bool(coverage.get('busy_switch_count'))
        checks['busy_switch'] = 'met' if busy else 'unmet'
        if not busy:
            reasons.append('busy_switch_not_observed')
    else:
        checks['busy_switch'] = 'not_required'
    checks['max_posts'] = ('met' if budget['posts_allowed'] else 'unmet')
    if checks['max_posts'] == 'unmet':
        reasons.append('measured_attempts_exceed_declared_max_posts')
    checks['max_tokens'] = ('met' if budget['max_tokens_allowed'] else 'unmet')
    if checks['max_tokens'] == 'unmet':
        reasons.append('declared_max_tokens_exceeded_or_missing')
    if statistics['complete_response_count'] != statistics['count']:
        reasons.append('not_all_responses_complete')
    if baseline_mismatches:
        reasons.append('identity_baseline_mismatch')
    # 只有至少声明了一个容量门时，才可能声称容量验收通过。
    declared = contract.expected_peak is not None or contract.require_busy_switch
    checks['collection_pass_is_not_capacity_pass'] = True
    return {
        'passed': bool(declared and not reasons and coverage.get('whole_test_lease_set_proven')),
        'declared_capacity_gate': declared,
        'checks': checks, 'reasons': reasons,
        'note': '容量验收须显式 expected_peak/require_busy_switch；集合等量证明不等价于容量通过',
    }


def build_report(cases, observations, prior_cursor, baseline=None, trace_id=None,
                 analysis_contract=None, mode=None):
    """核心：给定完整证据，产出可复用的一体化结果（不写盘）。"""
    _validate_cases(cases)
    if not isinstance(prior_cursor, int) or isinstance(prior_cursor, bool) or prior_cursor < 0:
        raise ValueError('prior_cursor_must_be_nonnegative_integer')
    if analysis_contract is None:
        analysis_contract = AnalysisContract()

    events = observations.get('events')
    samples = observations.get('samples')
    if not isinstance(events, list) or not isinstance(samples, list):
        raise ValueError('observations_must_carry_events_and_samples')
    completeness = observations.get('event_completeness_readback')
    if isinstance(completeness, dict):
        merged = dict(observations)
        merged['event_completeness_readback'] = completeness
    else:
        merged = observations

    mode = mode or derive_mode(cases, events)
    host_ids = {row.get('host_request_id') for row in cases} - {None}
    # host 模式按 host id 过滤；legacy 集合证据没有 host id，必须用完整 all-events。
    pool = observe_pool(events, mode='test' if mode == MODE_HOST else 'all',
                        host_ids=host_ids)
    coverage = prove_coverage(cases, merged, prior_cursor, pool, mode=mode)
    statistics = response_statistics(cases)

    identity_expectation = None
    expected_baseline, case_mismatches = _attach_baseline(cases)
    baseline_mismatches = list(case_mismatches)
    if baseline is not None:
        if not isinstance(baseline, dict):
            raise ValueError('baseline_must_be_object')
        identity_expectation = {'baseline': baseline}
        for index, row in enumerate(cases):
            identity = row.get('identity')
            if not isinstance(identity, dict) or not identity:
                baseline_mismatches.append('case_identity_missing:%s' % index)
            else:
                baseline_mismatches.extend('case_%s:%s' % (index, field) for field in
                                           compare_identity(fingerprint_identity(identity), baseline))

    trace_link = None
    if trace_id is not None:
        trace_link = connect_trace(cases, trace_id, events=events)

    measured_attempts = _measured_attempts(pool)

    def token_within(row):
        if analysis_contract.max_tokens is None:
            return True
        mt = row.get('max_tokens')
        return (isinstance(mt, int) and not isinstance(mt, bool)
                and 0 < mt <= analysis_contract.max_tokens)

    budget = {
        'measured_request_count': len(cases),
        'measured_attempt_count': measured_attempts,
        'posts_budget_basis': 'observed_acquire_attempts_not_authorized_posts',
        'posts_allowed': bool(_all_within(analysis_contract.max_posts, measured_attempts)),
        'max_tokens_allowed': bool(all(token_within(row) for row in cases)),
        'analysis_contract': {
            'expected_peak': analysis_contract.expected_peak,
            'require_busy_switch': analysis_contract.require_busy_switch,
            'max_posts': analysis_contract.max_posts,
            'max_tokens': analysis_contract.max_tokens,
        },
        'per_request_trace_present': all(row.get('client_trace_id') for row in cases),
        'per_request_host_id_present': all(row.get('host_request_id') for row in cases),
    }

    contract = _evaluate_contract(coverage, statistics, analysis_contract,
                                  baseline_mismatches, budget)
    collection_proven = bool(coverage['whole_test_lease_set_proven']
                             and not baseline_mismatches
                             and statistics['complete_response_count'] == len(cases))
    budget_ok = budget['posts_allowed'] and budget['max_tokens_allowed']
    capacity_blocked = (contract['checks'].get('peak') == 'unmet'
                        or contract['checks'].get('busy_switch') == 'unmet')

    return {
        'tool': 'regression-analyze',
        'generated_at_utc': _now(),
        'mode': mode,
        'inputs': {
            'case_count': len(cases),
            'prior_cursor': prior_cursor,
            'baseline_provided': baseline is not None,
            'baseline_mismatched_fields': sorted(baseline_mismatches, key=str),
            'identity_fingerprint_sha256': canonical_sha256(expected_baseline)
                                         if expected_baseline else None,
        },
        'budget': budget,
        'response_statistics': statistics,
        'pool_observation': pool,
        'coverage_proof': coverage,
        'capacity_contract': contract,
        'trace_link': trace_link,
        # collection_proven 独立保留；passed 是整体门（集合+预算+显式容量门+身份）。
        'collection_proven': collection_proven,
        'capacity_acceptance_passed': contract['passed'],
        'passed': bool(collection_proven and budget_ok and not capacity_blocked),
    }


def analyze_files(case_path, observations_path, prior_cursor, baseline_path=None,
                  trace_id=None, completeness_path=None, analysis_contract=None):
    """从明确路径载入证据并产出报告；供CLI与测试共用，便于自包含复用。"""
    cases = load_json(case_path)
    if isinstance(cases, dict) and 'cases' in cases:
        cases = cases['cases']
    observations = load_json(observations_path)
    if completeness_path is not None:
        if not isinstance(observations, dict):
            raise ValueError('observations_must_be_object_to_merge_completeness')
        observations = dict(observations)
        observations['event_completeness_readback'] = load_json(completeness_path)
    baseline = load_json(baseline_path) if baseline_path is not None else None
    report = build_report(cases, observations, prior_cursor, baseline=baseline,
                          trace_id=trace_id, analysis_contract=analysis_contract)
    report['inputs'].update({
        'case_file_sha256': file_sha256(case_path),
        'observations_file_sha256': file_sha256(observations_path),
        'baseline_file_sha256': file_sha256(baseline_path),
        'completeness_file_sha256': file_sha256(completeness_path),
    })
    return report


def main(argv=None):
    """analyze 子命令入口：离线读明确JSON证据，输出新结果，拒绝覆盖。"""
    parser = argparse.ArgumentParser(
        prog='regression.sh analyze',
        description='离线分析既有JSON证据，不访问生产或真实模型。')
    parser.add_argument('--cases', required=True,
                        help='请求记录JSON（host_rows数组或含cases字段的对象）')
    parser.add_argument('--observations', required=True, help='脱敏事件与采样JSON')
    parser.add_argument('--completeness', help='单独的事件完整性回读JSON（可缺省，若观测已内嵌）')
    parser.add_argument('--prior-cursor', type=int, required=True,
                        help='本轮开始前的request事件游标，必须来自真实回读')
    parser.add_argument('--baseline', help='已核对的身份基线JSON，用于核对漂移')
    parser.add_argument('--trace-id', help='可信标准UUID，直接连接client trace与host request_id')
    parser.add_argument('--expected-peak', type=int, default=None,
                        help='声明期望观测峰值；未给出则不声称任何容量通过')
    parser.add_argument('--require-busy-switch', action='store_true',
                        help='要求观测到满载重选；未给出则不声称该能力')
    parser.add_argument('--max-posts', type=int, default=None, help='允许的最大请求数；超出即失败')
    parser.add_argument('--max-tokens', type=int, default=None, help='每条请求允许的最大max_tokens')
    parser.add_argument('--out', required=True, help='输出JSON路径；已存在则拒绝覆盖')
    args = parser.parse_args(argv)

    try:
        contract = AnalysisContract(expected_peak=args.expected_peak,
                                    require_busy_switch=args.require_busy_switch,
                                    max_posts=args.max_posts, max_tokens=args.max_tokens)
        report = analyze_files(args.cases, args.observations, args.prior_cursor,
                               baseline_path=args.baseline, trace_id=args.trace_id,
                               completeness_path=args.completeness, analysis_contract=contract)
        atomic_write_new(args.out, report)
    except FileExistsError:
        print(json.dumps({'ok': False, 'failure': 'output_exists_refusing_to_overwrite'}))
        return 1
    except Exception as exc:  # noqa: BLE001 - 证据不足一律失败，不伪造通过
        print(json.dumps({'ok': False, 'failure': str(exc) if isinstance(exc, ValueError)
                          else type(exc).__name__}))
        return 1
    print(json.dumps({'ok': True, 'passed': report['passed'],
                      'collection_proven': report['collection_proven'],
                      'capacity_acceptance_passed': report['capacity_acceptance_passed'],
                      'mode': report['mode'], 'trace_link': report['trace_link'],
                      'out': args.out}))
    return 0 if report['passed'] else 2


if __name__ == '__main__':
    sys.exit(main())
