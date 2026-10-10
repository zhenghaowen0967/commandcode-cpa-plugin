"""显式授权的直接 CPA 单路探针；每次发送前检查真实运行身份，不自动重试。"""
import argparse
import datetime as dt
import json
import os
import sys
import time
from pathlib import Path

from evidence import (UUID_RE, atomic_write_new, atomic_replace_private,
                      canonical_sha256, load_json, read_private_file)
from live_wire import decode_message, post_once
from runtime_identity import capture_identity, management_get, validate_baseline

PROBE_MAX_REQUESTS = 4
PROBE_MAX_CONCURRENCY = 1
PROBE_MAX_TOKENS = 1024
PROBE_DEFAULT_TOKENS = 128
SYNTHETIC_PROMPT = '请只回复“请求追踪探针成功”，不要解释，不要使用任何工具。'
SYNTHETIC_MARKER = '请求追踪探针成功'
FIXED_MODEL = 'cc-deepseek-v4.1-flash'
SAFE_FAILURES = frozenset((
    'identity_changed_before_post', 'identity_changed_after_post',
    'identity_changed_during_preflight', 'model_http_failure', 'probe_marker_missing',
    'trace_association_not_proven', 'request_events_unavailable',
    'request_event_cursor_regressed', 'response_not_json', 'unexpected_reply_type',
    'response_blocks_invalid', 'probe_must_not_call_tools_or_unknown_blocks',
    'response_text_invalid', 'response_not_complete', 'response_usage_invalid',
    'response_exceeds_complete_capture_limit', 'management_read_failed',
    'management_response_not_object', 'process_executable_path_changed',
    'process_config_argument_missing', 'process_config_path_changed',
    'expected_library_not_mapped_or_deleted', 'mapped_library_file_identity_changed',
    'gateway_listener_not_owned_by_expected_cpa',
))


def _failure_name(exc):
    text = str(exc)
    if text in SAFE_FAILURES or text in {'identity_mismatch:' + field for field in (
            'pid', 'startticks_epoch', 'core_sha256', 'lib_sha256', 'config_sha256',
            'plugin_version', 'management_boundary')}:
        return text
    return type(exc).__name__


class LiveRefused(ValueError):
    """发送之前发现授权或参数不满足时拒绝，未消费模型请求预算。"""


def _budget(requests, max_tokens, concurrency=1):
    """明确拒绝越界或布尔预算，不把无效值静默夹到许可范围。"""
    for name, value, maximum in (('requests', requests, PROBE_MAX_REQUESTS),
                                  ('max_tokens', max_tokens, PROBE_MAX_TOKENS),
                                  ('concurrency', concurrency, PROBE_MAX_CONCURRENCY)):
        if isinstance(value, bool) or not isinstance(value, int) or not 1 <= value <= maximum:
            raise LiveRefused('budget_out_of_range:' + name)


def authorize(args):
    """授权标记与所有显式预算必须齐全，才允许读取运行基线和秘密。"""
    if not args.authorized_live:
        raise LiveRefused('explicit_authorization_flag_required')
    _budget(args.requests, args.max_tokens, args.concurrency)
    for field in ('gateway', 'key_file', 'management_key_file', 'identity_baseline', 'journal', 'out'):
        if not getattr(args, field, None):
            raise LiveRefused('argument_required:' + field)
    if Path(args.out).resolve() == Path(args.journal).resolve():
        raise LiveRefused('output_and_journal_must_differ')
    if os.path.lexists(args.journal) or os.path.lexists(args.out):
        raise LiveRefused('journal_or_output_exists_refusing_to_repost')
    return args


def open_journal(path, plan):
    """独占登记当轮预算，旧 journal 一律拒绝，不从历史轮次恢复重发。"""
    record = {'tool': 'regression-live-probe', 'opened_at_utc': _now(),
              'plan': plan, 'posts_sent': 0, 'posts_reserved': 0, 'budget_committed': True,
              'full_capacity_supported': False, 'progress': []}
    atomic_write_new(path, record)
    return record


def _now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def _save(path, record):
    """先写私有临时文件再原子替换，保证预算先于模型 POST 持久化。"""
    atomic_replace_private(path, record)


def _events(host, port, prefix, key, after=0):
    """仅在受确认的同一 CPA 查询请求事件，不落盘账号标识或完整管理响应。"""
    data = management_get(host, port, prefix + '/events?scope=requests&after=' + str(after), key)
    if not isinstance(data.get('events'), list):
        raise ValueError('request_events_unavailable')
    cursor = int(data.get('next_cursor', after))
    if cursor < after:
        raise ValueError('request_event_cursor_regressed')
    return data['events'], cursor


def trace_association(trace, events):
    """只通过事件中精确相等的 trace 核对 host 和 owner，不按时间猜关联。"""
    linked = [event for event in events if trace and event.get('trace_id') == trace]
    hosts = {event.get('request_id') for event in linked}
    lifecycle = [event for event in events if event.get('request_id') in hosts
                 and event.get('action') in ('pick', 'acquire', 'acquire_rejected', 'quarantined', 'settle')]
    acquires = [event for event in linked if event.get('action') == 'acquire']
    settles = [event for event in linked if event.get('action') == 'settle']
    tokens = {(event.get('request_id'), event.get('attempt_id')) for event in acquires}
    completed = {(event.get('request_id'), event.get('attempt_id')) for event in settles}
    verified = bool(linked and len(hosts) == 1 and None not in hosts and '' not in hosts
                    and len(acquires) == 1 and len(settles) == 1 and tokens == completed
                    and all(event.get('attempt_id') for event in acquires)
                    and all(event.get('reason') == 'upstream_complete' for event in settles)
                    and all(event.get('trace_id') == trace for event in lifecycle)
                    and any(event.get('action') == 'pick' for event in linked))
    return {'trace_association_verified': verified,
            'host_request_id': next(iter(hosts)) if verified else None,
            'natural_settle_count': len(settles) if verified else 0}


def run_probe(gateway, key, requests, max_tokens, journal_path, *, baseline,
              management_key, require_trace=False):
    """顺序执行当轮探针；身份漂移或首次失败停追加，已经发出的响应完整读取。"""
    _budget(requests, max_tokens)
    host, port, prefix = validate_baseline(baseline, gateway)
    try:
        identity = capture_identity(baseline, gateway, management_key)
    except Exception as exc:
        raise LiveRefused('initial_identity_preflight_failed:' + _failure_name(exc)) from exc
    record = open_journal(journal_path, {'gateway': gateway, 'requests': requests,
                                       'concurrency': 1, 'max_tokens': max_tokens,
                                       'model': FIXED_MODEL, 'require_trace': require_trace,
                                       'baseline_sha256': canonical_sha256(baseline)})
    cases, failure = [], None
    for index in range(requests):
        case = {'index': index, 'attempt_status': 'not_sent'}
        try:
            _, cursor = _events(host, port, prefix, management_key)
            current = capture_identity(baseline, gateway, management_key)
            if current != identity:
                raise ValueError('identity_changed_before_post')
            body = {'model': FIXED_MODEL, 'max_tokens': max_tokens, 'stream': False,
                    'messages': [{'role': 'user', 'content': SYNTHETIC_PROMPT}]}
            case.update({'started_at_utc': _now(), 'attempt_status': 'budget_reserved'})
            record['posts_reserved'] += 1
            record['progress'].append(dict(case))
            _save(journal_path, record)
            record['posts_sent'] += 1
            case['attempt_status'] = 'started'
            started = time.monotonic()
            status, content_type, raw, traces = post_once(host, port, '/v1/messages', key, body)
            case.update({'http': status, 'elapsed_seconds': time.monotonic() - started,
                         'finished_at_utc': _now(), 'content_type': content_type,
                         'response_trace_ids': [value for value in traces if UUID_RE.fullmatch(value)]})
            if status != 200:
                raise ValueError('model_http_failure')
            decoded = decode_message(raw)
            if SYNTHETIC_MARKER not in decoded['text']:
                raise ValueError('probe_marker_missing')
            case.update({'stop_reason': decoded['stop_reason'],
                         'text_characters': decoded['text_characters'],
                         **decoded['usage_summary']})
            current = capture_identity(baseline, gateway, management_key)
            if current != identity:
                raise ValueError('identity_changed_after_post')
            events, _ = _events(host, port, prefix, management_key, cursor)
            trace = traces[0] if len(traces) == 1 and UUID_RE.fullmatch(traces[0]) else None
            case['client_trace_id'] = trace
            case.update(trace_association(trace, events))
            if require_trace and not case['trace_association_verified']:
                raise ValueError('trace_association_not_proven')
            case['attempt_status'] = 'verified'
        except Exception as exc:
            case['failure'] = _failure_name(exc)
            case['attempt_status'] = 'failed' if case.get('started_at_utc') else 'not_sent'
            case['finished_at_utc'] = _now()
            failure = case['failure']
        cases.append(case)
        if record['progress'] and case.get('started_at_utc'):
            record['progress'][-1] = dict(case)
        record['cases'] = cases
        _save(journal_path, record)
        if failure:
            break
    record.update({'finished_at_utc': _now(), 'failure': failure})
    _save(journal_path, record)
    passed = len(cases) == requests and all(case['attempt_status'] == 'verified' for case in cases)
    return {'passed': passed, 'failure': failure, 'cases': cases,
            'posts_sent': record['posts_sent'], 'posts_reserved': record['posts_reserved'],
            'full_capacity_supported': False,
            'identity_source': 'live_kernel_and_same_cpa_management',
            'identity_sha256': canonical_sha256(identity)}


def main(argv=None):
    """真实模式只用于直接 CPA；授权、实际身份、输出预留和预算均先于模型 POST。"""
    parser = argparse.ArgumentParser(prog='regression.sh live')
    parser.add_argument('--authorized-live', action='store_true')
    parser.add_argument('--gateway')
    parser.add_argument('--key-file')
    parser.add_argument('--management-key-file')
    parser.add_argument('--identity-baseline')
    parser.add_argument('--journal')
    parser.add_argument('--out')
    parser.add_argument('--requests', type=int)
    parser.add_argument('--concurrency', type=int)
    parser.add_argument('--max-tokens', type=int)
    parser.add_argument('--require-trace', action='store_true')
    args = parser.parse_args(argv)
    output_reserved, run_started = False, False
    try:
        authorize(args)
        baseline = load_json(args.identity_baseline)
        validate_baseline(baseline, args.gateway)
        key = read_private_file(args.key_file)
        management_key = read_private_file(args.management_key_file)
        if not key or not management_key:
            raise LiveRefused('key_empty')
        atomic_write_new(args.out, {'passed': False, 'state': 'prepared', 'posts_sent': 0})
        output_reserved = True
        run_started = True
        result = run_probe(args.gateway, key, args.requests, args.max_tokens, args.journal,
                           baseline=baseline, management_key=management_key,
                           require_trace=args.require_trace)
        atomic_replace_private(args.out, result)
    except Exception as exc:
        failure = str(exc) if isinstance(exc, LiveRefused) else type(exc).__name__
        result = {'passed': False, 'failure': failure,
                  'posts_sent': 0 if isinstance(exc, LiveRefused) or not run_started else None,
                  'full_capacity_supported': False}
        if output_reserved:
            atomic_replace_private(args.out, result)
        print(json.dumps(result))
        return 1
    print(json.dumps({'passed': result['passed'], 'failure': result['failure'],
                      'posts_sent': result['posts_sent'], 'full_capacity_supported': False,
                      'out': args.out}))
    return 0 if result['passed'] else 2


if __name__ == '__main__':
    sys.exit(main())
