"""自包含的公开合成夹具：只含合成UUID与数值，不含生产路径、账号或密钥。

这些夹具用于离线单测与 analyze 端到端检查，可脱离本机 scratch 独立复用。
"""
import copy
import json

MODEL = 'cc-deepseek-v4.1-flash'

# 合成但形状合法的标准UUID，避免与真实证据混淆。
TRACE_A = '56b71bea-68de-4d97-9ac6-ec1f156c65c7'
TRACE_B = '7c3f2a10-1111-4222-8333-444455556666'
TRACE_REUSE = '2ea2cba8-f4b3-4450-bd8f-e35072c8772f'
HOST_A = 'host-aaaa-0001'
HOST_B = 'host-bbbb-0002'
# usage 侧的 request_id 是客户端 trace，与宿主 ID 不同：绝不能当 host id 用。
USAGE_A = 'aaaa0000-0000-4000-8000-000000000001'
USAGE_B = 'bbbb0000-0000-4000-8000-000000000002'


def identity(**overrides):
    """一份合成的身份快照（全部为摘要/数字，不含明文身份）。"""
    base = {
        'pid': 1234,
        'startticks_epoch': '1234:5678',
        'core_sha256': 'a' * 64,
        'lib_sha256': 'b' * 64,
        'config_sha256': 'c' * 64,
        'plugin_version': '0.1.7-local',
        'management_boundary': 'loopback-management',
        'identity_confirmed': True,
    }
    base.update(overrides)
    return base


def cases():
    """两条被测请求：唯一宿主ID、唯一可信trace、边界时间与账号序号齐全。"""
    return [
        {
            'host_request_id': HOST_A,
            'client_trace_id': TRACE_A,
            'host_request_id_is_client_fabricable': False,
            'attempt_status': 'verified',
            'http': 200,
            'elapsed_seconds': 1.25,
            'started_at_utc': '2026-10-10T00:00:00.010Z',
            'finished_at_utc': '2026-10-10T00:00:01Z',
            'server_execution': {'account_ordinal': 1},
            'identity': identity(),
        },
        {
            'host_request_id': HOST_B,
            'client_trace_id': TRACE_B,
            'host_request_id_is_client_fabricable': False,
            'attempt_status': 'verified',
            'http': 200,
            'elapsed_seconds': 2.0,
            'started_at_utc': '2026-10-10T00:00:00.020Z',
            'finished_at_utc': '2026-10-10T00:00:01.500Z',
            'server_execution': {'account_ordinal': 1},
            'identity': identity(),
        },
    ]


def _event(sequence, action, host, attempt, group=1, reason=None, trace=None, **extra):
    return {
        'sequence': sequence,
        'request_id': host,
        'attempt_id': attempt,
        'model': MODEL,
        'action': action,
        'reason': reason if reason is not None
                  else ('upstream_complete' if action == 'settle' else 'acquired'),
        'account_ordinal': group,
        'trace_id': trace,
        **extra,
    }


def observations():
    """两条独立lease的完整事件流、边界采样与事件完整性回读。

    每个 host 的 owner 关键事件（pick/acquire/settle）都带同一个 trace_id，
    供逐条连接与“全 owner 事件 trace 一致”的核对。
    """
    events = [
        _event(11, 'pick', HOST_A, None, reason='selected', trace=TRACE_A),
        _event(12, 'acquire', HOST_A, 'a1', trace=TRACE_A),
        _event(13, 'pick', HOST_B, None, reason='selected', trace=TRACE_B),
        _event(14, 'acquire', HOST_B, 'b1', trace=TRACE_B),
        _event(15, 'settle', HOST_A, 'a1', trace=TRACE_A),
        _event(16, 'settle', HOST_B, 'b1', trace=TRACE_B),
    ]
    samples = [
        {'at': '2026-10-10T00:00:00Z',
         'accounts': [{'account_ordinal': 1, 'inflight': 0, 'cap': 10, 'status': 'eligible'}]},
        {'at': '2026-10-10T00:00:02Z',
         'accounts': [{'account_ordinal': 1, 'inflight': 0, 'cap': 10, 'status': 'eligible'}]},
    ]
    completeness = {
        'at_utc': '2026-10-10T00:00:03Z',
        'initial_cursor': 10,
        'last_test_event_sequence': 16,
        'oldest_retained_request_sequence': 5,
        'retained_request_events': 20,
        'period_request_events': 6,
        'saved_period_events': 6,
        'period_events_equal_exactly': True,
        'actual_buffer_retained_entire_period': True,
    }
    return {'epoch': '1234:5678', 'events': events, 'samples': samples,
            'event_completeness_readback': completeness}


def legacy_cases():
    """旧 0.1.7 集合证据：没有 host_request_id，用 session_id + 最终唯一成功 usage 证明。

    刻意保留 usage request_id（客户端 trace）与宿主 ID 不同的事实，绝不回填 host UUID。
    """
    return [
        {
            'session_id': 'session-public-a',
            'association': 'exact_session',
            'attempt_status': 'verified',
            'http': 200,
            'elapsed_seconds': 1.5,
            'started_at_utc': '2026-10-10T00:00:00.010Z',
            'finished_at_utc': '2026-10-10T00:00:01Z',
            'server_execution': {'account_ordinal': 1},
            'cpa_usage': [{'request_id': USAGE_A, 'failed': 0, 'fail_status_code': 200}],
            'identity': identity(),
        },
        {
            'session_id': 'session-public-b',
            'association': 'exact_session',
            'attempt_status': 'verified',
            'http': 200,
            'elapsed_seconds': 2.0,
            'started_at_utc': '2026-10-10T00:00:00.020Z',
            'finished_at_utc': '2026-10-10T00:00:01.500Z',
            'server_execution': {'account_ordinal': 1},
            'cpa_usage': [{'request_id': USAGE_B, 'failed': 0, 'fail_status_code': 200}],
            'identity': identity(),
        },
    ]


def legacy_cases_with_cap_rejection():
    """一个 session 含“本地cap拒绝 + 最终成功”两条 usage，模拟真实满载重试。"""
    cases = legacy_cases()
    cases[0]['cpa_usage'] = [
        {'request_id': USAGE_A, 'failed': 1, 'fail_status_code': 429, 'latency_ms': 0,
         'confirmed_local_cap_rejection': True},
        {'request_id': USAGE_A, 'failed': 0, 'fail_status_code': 200},
    ]
    return cases


def legacy_observations():
    """旧证据观测：host 侧只有不透明 UUID，没有逐条 trace。"""
    events = observations()
    for event in events['events']:
        event['trace_id'] = None
    return events


def prior_cursor():
    return 10


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding='utf-8')
    return path


def clone(value):
    return copy.deepcopy(value)
