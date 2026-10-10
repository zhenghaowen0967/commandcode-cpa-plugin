"""live 探针的离线回归：默认拒发、预算/独占门、零重试、真实 capture、计费账本。

全部用 mock 替换传输与身份层；本测试不发出任何真实网络请求、不读取任何真实凭据，
也不访问真实 /proc。授权门、预算门、journal 独占、逐轮 capture 与 posts_sent/reserved
账本都在本文件内以公开合成值验证。
"""
import contextlib
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import live  # noqa: E402

GATEWAY = 'http://127.0.0.1:15721'
BOUNDARY = 'http://127.0.0.1:15721/v0/management/plugins/commandcode-pool'
PUBLIC_KEY = 'public-live-key-token'
PUBLIC_MGMT = 'public-management-token'
TRACE = '56b71bea-68de-4d97-9ac6-ec1f156c65c7'
TRACE_OTHER = '7c3f2a10-1111-4222-8333-444455556666'
HOST = 'host-aaaa-0001'

# capture_identity 的合成返回值（与被测基线一致）；不含任何明文身份。
IDENTITY = {
    'pid': 4321,
    'startticks_epoch': '4321:98765',
    'core_sha256': 'a' * 64,
    'lib_sha256': 'b' * 64,
    'config_sha256': 'c' * 64,
    'plugin_version': '0.1.8-local',
    'management_boundary': BOUNDARY,
}


def baseline_identity(**overrides):
    base = {
        'pid': 4321,
        'startticks_epoch': '4321:98765',
        'core_sha256': 'a' * 64,
        'lib_sha256': 'b' * 64,
        'config_sha256': 'c' * 64,
        'plugin_version': '0.1.8-local',
        'management_boundary': BOUNDARY,
        'core_path': '/opt/cpa/commandcode-pool-next',
        'library_path': '/opt/cpa/lib/commandcode-pool-next.so',
        'config_path': '/opt/cpa/config.yaml',
        'identity_confirmed': True,
    }
    base.update(overrides)
    return base


def reply(marker=True):
    text = live.SYNTHETIC_MARKER if marker else '别的文本'
    return json.dumps({'type': 'message', 'content': [{'type': 'text', 'text': text}],
                       'stop_reason': 'end_turn',
                       'usage': {'input_tokens': 10, 'output_tokens': 4}}).encode()


def events_ok(trace=TRACE, host=HOST, attempt='a1', settle_reason='upstream_complete',
              include_pick=True):
    events = [
        {'sequence': 2, 'request_id': host, 'attempt_id': attempt, 'action': 'acquire',
         'trace_id': trace},
        {'sequence': 3, 'request_id': host, 'attempt_id': attempt, 'action': 'settle',
         'reason': settle_reason, 'trace_id': trace},
    ]
    if include_pick:
        events.insert(0, {'sequence': 1, 'request_id': host, 'attempt_id': attempt,
                          'action': 'pick', 'trace_id': trace})
    return events


class FakeArgs:
    def __init__(self, **overrides):
        self.authorized_live = True
        self.requests = 1
        self.concurrency = 1
        self.max_tokens = 128
        self.gateway = GATEWAY
        self.key_file = '/tmp/key'
        self.management_key_file = '/tmp/mgmt-key'
        self.journal = '/tmp/journal.json'
        self.identity_baseline = '/tmp/baseline.json'
        self.out = '/tmp/out.json'
        for key, value in overrides.items():
            setattr(self, key, value)


def run(journal, *, requests=1, max_tokens=128, require_trace=False, identity=None,
        capture_seq=None, event_seq=None, post_seq=None, baseline=None, gateway=GATEWAY):
    """在 mock 传输/身份层下调用 run_probe，返回 (result, capture_mock, post_mock)。"""
    baseline = baseline_identity() if baseline is None else baseline
    identity = IDENTITY if identity is None else identity
    if event_seq is None:
        event_seq = [(events_ok(TRACE), 5)] * (2 * requests + 2)
    capture = mock.Mock(side_effect=capture_seq) if capture_seq is not None \
        else mock.Mock(return_value=identity)
    events = mock.Mock(side_effect=event_seq)
    post = mock.Mock(side_effect=post_seq) if post_seq is not None \
        else mock.Mock(return_value=(200, 'application/json', reply(), [TRACE]))
    with mock.patch.object(live, 'capture_identity', capture), \
            mock.patch.object(live, '_events', events), \
            mock.patch.object(live, 'post_once', post):
        result = live.run_probe(gateway, PUBLIC_KEY, requests, max_tokens, journal,
                                baseline=baseline, management_key=PUBLIC_MGMT,
                                require_trace=require_trace)
    return result, capture, post


def assert_no_secret(case, value):
    encoded = json.dumps(value, ensure_ascii=False)
    for secret in (PUBLIC_KEY, PUBLIC_MGMT, 'public-secret-marker'):
        case.assertNotIn(secret, encoded)


class AuthorizationTests(unittest.TestCase):
    def test_default_without_flag_refuses_with_zero_scope(self):
        with self.assertRaises(live.LiveRefused) as caught:
            live.authorize(FakeArgs(authorized_live=False))
        self.assertEqual(str(caught.exception), 'explicit_authorization_flag_required')

    def test_budget_rejects_bool_and_out_of_range_strictly(self):
        for bad in (FakeArgs(requests=True), FakeArgs(max_tokens=True),
                    FakeArgs(concurrency=True), FakeArgs(requests=0), FakeArgs(requests=5),
                    FakeArgs(max_tokens=0), FakeArgs(max_tokens=1025),
                    FakeArgs(max_tokens=999999), FakeArgs(concurrency=0),
                    FakeArgs(concurrency=2)):
            with self.subTest(bad=vars(bad)), self.assertRaises(live.LiveRefused):
                live.authorize(bad)
        args = FakeArgs()
        self.assertIs(live.authorize(args), args)

    def test_every_required_argument_is_enforced(self):
        for field in ('gateway', 'key_file', 'management_key_file', 'identity_baseline',
                      'journal', 'out'):
            with self.subTest(field=field), self.assertRaises(live.LiveRefused) as caught:
                live.authorize(FakeArgs(**{field: None}))
            self.assertEqual(str(caught.exception), 'argument_required:' + field)

    def test_output_and_journal_must_be_distinct_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = str(Path(directory) / 'j.json')
            same = str(Path(directory) / '.' / 'j.json')
            with self.assertRaises(live.LiveRefused) as caught:
                live.authorize(FakeArgs(journal=journal, out=same))
            self.assertEqual(str(caught.exception), 'output_and_journal_must_differ')

    def test_existing_journal_or_out_is_refused_before_any_post(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            journal = directory / 'j.json'
            out = directory / 'out.json'
            journal.write_text('{}')
            with self.assertRaises(live.LiveRefused) as caught:
                live.authorize(FakeArgs(journal=str(journal), out=str(out)))
            self.assertEqual(str(caught.exception), 'journal_or_output_exists_refusing_to_repost')
            out.write_text('{}')
            journal.unlink()
            with self.assertRaises(live.LiveRefused):
                live.authorize(FakeArgs(journal=str(journal), out=str(out)))


class JournalTests(unittest.TestCase):
    def test_journal_is_exclusive_private_and_reserves_budget(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            record = live.open_journal(journal, {'requests': 1})
            self.assertTrue(record['budget_committed'])
            self.assertEqual(record['posts_sent'], 0)
            self.assertEqual(record['posts_reserved'], 0)
            self.assertEqual(journal.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                live.open_journal(journal, {'requests': 1})

    def test_existing_journal_blocks_run_probe_without_posting(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            journal.write_text('{}')
            with mock.patch.object(live, 'capture_identity', mock.Mock(return_value=IDENTITY)), \
                    mock.patch.object(live, 'post_once',
                                      return_value=(200, 'application/json', reply(), [TRACE])) as wire:
                with self.assertRaises(FileExistsError):
                    live.run_probe(GATEWAY, PUBLIC_KEY, 1, 128, journal,
                                   baseline=baseline_identity(), management_key=PUBLIC_MGMT)
            self.assertEqual(wire.call_count, 0)


class ProbeTests(unittest.TestCase):
    def test_single_request_verifies_and_persists_private_ledger(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, capture, post = run(journal)
            self.assertTrue(result['passed'])
            self.assertEqual(result['posts_sent'], 1)
            self.assertEqual(result['posts_reserved'], 1)
            self.assertFalse(result['full_capacity_supported'])
            self.assertEqual(result['cases'][0]['attempt_status'], 'verified')
            # 真实身份层被逐轮调用，而不是读两份静态 JSON：初始 + POST 前 + POST 后。
            self.assertEqual(capture.call_count, 3)
            self.assertEqual(capture.call_args[0][0], baseline_identity())
            self.assertEqual(post.call_count, 1)
            self.assertEqual(post.call_args[0][:3], ('127.0.0.1', 15721, '/v1/messages'))
            saved = json.loads(journal.read_text())
            self.assertEqual(saved['posts_sent'], 1)
            self.assertEqual(saved['posts_reserved'], 1)
            self.assertEqual(journal.stat().st_mode & 0o777, 0o600)
            assert_no_secret(self, result)

    def test_two_and_four_requests_succeed_in_sequence(self):
        for count in (2, 4):
            with self.subTest(count=count), tempfile.TemporaryDirectory() as directory:
                journal = Path(directory) / 'live.json'
                result, _, post = run(journal, requests=count)
                self.assertTrue(result['passed'])
                self.assertEqual(result['posts_sent'], count)
                self.assertEqual(result['posts_reserved'], count)
                self.assertEqual(post.call_count, count)
                self.assertTrue(all(c['attempt_status'] == 'verified' for c in result['cases']))

    def test_http_429_counts_one_post_and_stops_without_retry(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, post = run(journal, requests=3,
                                  post_seq=[(429, 'application/json', b'{}', [])])
            self.assertEqual(post.call_count, 1)
            self.assertEqual(result['posts_sent'], 1)
            self.assertEqual(result['posts_reserved'], 1)
            self.assertEqual(result['cases'][0]['attempt_status'], 'failed')
            self.assertEqual(result['cases'][0]['failure'], 'model_http_failure')
            self.assertFalse(result['passed'])

    def test_marker_missing_is_failure_not_success(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, post = run(journal, post_seq=[
                (200, 'application/json', reply(marker=False), [TRACE])])
            self.assertEqual(post.call_count, 1)
            self.assertEqual(result['posts_sent'], 1)
            self.assertEqual(result['cases'][0]['failure'], 'probe_marker_missing')

    def test_transport_timeout_counts_one_post_and_stops(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, post = run(journal, requests=3,
                                  post_seq=[TimeoutError('public-secret-marker')])
            self.assertEqual(post.call_count, 1)
            self.assertEqual(result['posts_sent'], 1)
            self.assertEqual(result['posts_reserved'], 1)
            self.assertEqual(result['cases'][0]['attempt_status'], 'failed')
            # 只有固定异常类型名，不回显可能含机密的异常文本。
            self.assertEqual(result['cases'][0]['failure'], 'TimeoutError')
            assert_no_secret(self, result)

    def test_second_round_capture_drift_stops_before_further_posts(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            drifted = dict(IDENTITY, pid=9999)
            result, capture, post = run(journal, requests=2,
                                        capture_seq=[IDENTITY, IDENTITY, IDENTITY, drifted])
            self.assertEqual(post.call_count, 1)  # 第二轮漂移，停止后续
            self.assertEqual(result['posts_sent'], 1)
            self.assertEqual(result['failure'], 'identity_changed_before_post')
            self.assertEqual(result['cases'][1]['attempt_status'], 'not_sent')
            self.assertFalse(result['passed'])

    def test_capture_drift_after_post_is_detected(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, post = run(journal,
                                  capture_seq=[IDENTITY, IDENTITY, dict(IDENTITY, pid=9999)])
            self.assertEqual(post.call_count, 1)
            self.assertEqual(result['cases'][0]['failure'], 'identity_changed_after_post')
            self.assertEqual(result['posts_sent'], 1)

    def test_journal_save_failure_before_wire_is_reserved_not_sent(self):
        """预算已预占但落盘失败、尚未调用传输：不得谎报已 POST。"""
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            real_save = live._save
            calls = {'n': 0}

            def flaky(path, record):
                calls['n'] += 1
                if calls['n'] == 1:
                    raise OSError('public-secret-marker')
                return real_save(path, record)

            with mock.patch.object(live, '_save', flaky):
                result, _, post = run(journal)
            self.assertEqual(post.call_count, 0)
            self.assertEqual(result['posts_sent'], 0)
            self.assertEqual(result['posts_reserved'], 1)
            self.assertEqual(result['cases'][0]['failure'], 'OSError')
            saved = json.loads(journal.read_text())
            self.assertEqual(saved['posts_reserved'], 1)
            self.assertEqual(saved['posts_sent'], 0)
            assert_no_secret(self, result)

    def test_crash_after_reservation_leaves_ambiguous_budget_and_blocks_repost(self):
        """预占后崩溃：journal 记为 budget_reserved/posts_sent=0，可能已发出，不得重发。"""
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            with mock.patch.object(live, 'post_once', side_effect=KeyboardInterrupt):
                with self.assertRaises(KeyboardInterrupt):
                    run(journal, post_seq=[KeyboardInterrupt])
            saved = json.loads(journal.read_text())
            self.assertEqual(saved['posts_reserved'], 1)
            self.assertEqual(saved['posts_sent'], 0)
            self.assertEqual(saved['progress'][-1]['attempt_status'], 'budget_reserved')
            # 未完成 journal 存在时，任何授权请求都被拒绝，不得假定未发而重发。
            with self.assertRaises(live.LiveRefused):
                live.authorize(FakeArgs(journal=str(journal), out=str(journal.parent / 'out.json')))

    def test_remote_gateway_is_refused_before_journal(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            with self.assertRaises(ValueError):
                run(journal, gateway='http://api.example.com')
            self.assertFalse(journal.exists())


class RequireTraceTests(unittest.TestCase):
    def test_require_trace_needs_exact_header_and_event_trace(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, _ = run(journal, require_trace=True)
            self.assertTrue(result['passed'])
            self.assertTrue(result['cases'][0]['trace_association_verified'])
            self.assertEqual(result['cases'][0]['host_request_id'], HOST)

    def test_missing_header_is_not_proven(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, _ = run(journal, require_trace=True,
                               post_seq=[(200, 'application/json', reply(), [])])
            self.assertEqual(result['cases'][0]['failure'], 'trace_association_not_proven')
            self.assertFalse(result['cases'][0]['trace_association_verified'])

    def test_header_trace_not_matching_events_is_not_proven(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, _ = run(journal, require_trace=True,
                               post_seq=[(200, 'application/json', reply(), [TRACE_OTHER])])
            self.assertEqual(result['cases'][0]['failure'], 'trace_association_not_proven')

    def test_duplicated_or_non_uuid_header_is_ambiguous(self):
        for traces in ([TRACE, TRACE], ['not-a-uuid']):
            with self.subTest(traces=traces), tempfile.TemporaryDirectory() as directory:
                journal = Path(directory) / 'live.json'
                result, _, _ = run(journal, require_trace=True, post_seq=[
                    (200, 'application/json', reply(), traces)])
                self.assertEqual(result['cases'][0]['failure'], 'trace_association_not_proven')

    def test_incomplete_lifecycle_in_events_is_not_proven(self):
        # 即使 header 与 events 的 trace 相同，缺 pick、非自然 settle、或多 host 都不算证据。
        for events in (events_ok(include_pick=False),
                       events_ok(settle_reason='cancelled'),
                       events_ok() + [{'sequence': 4, 'request_id': 'host-other',
                                       'attempt_id': 'a1', 'action': 'acquire', 'trace_id': TRACE}]):
            with self.subTest(events=events), tempfile.TemporaryDirectory() as directory:
                journal = Path(directory) / 'live.json'
                result, _, _ = run(journal, require_trace=True,
                                   event_seq=[(events, 5)] * 4)
                self.assertEqual(result['cases'][0]['failure'], 'trace_association_not_proven')

    def test_without_require_trace_fake_header_is_not_evidence_but_still_verified(self):
        with tempfile.TemporaryDirectory() as directory:
            journal = Path(directory) / 'live.json'
            result, _, _ = run(journal, post_seq=[
                (200, 'application/json', reply(), [TRACE_OTHER])])
            self.assertTrue(result['passed'])
            self.assertFalse(result['cases'][0]['trace_association_verified'])
            self.assertIsNone(result['cases'][0]['host_request_id'])


def write_private(path, text):
    path.write_text(text, encoding='utf-8')
    path.chmod(0o600)
    return path


def live_argv(directory, *, requests=1, require_trace=False, gateway=GATEWAY):
    key_file = write_private(directory / 'key', PUBLIC_KEY)
    mgmt_file = write_private(directory / 'mgmt-key', PUBLIC_MGMT)
    baseline_file = directory / 'baseline.json'
    baseline_file.write_text(json.dumps(baseline_identity()), encoding='utf-8')
    argv = ['--authorized-live', '--gateway', gateway,
            '--key-file', str(key_file), '--management-key-file', str(mgmt_file),
            '--identity-baseline', str(baseline_file),
            '--journal', str(directory / 'journal.json'),
            '--out', str(directory / 'out.json'),
            '--requests', str(requests), '--concurrency', '1', '--max-tokens', '128']
    if require_trace:
        argv.append('--require-trace')
    return argv


def call_main(argv):
    buffer = io.StringIO()
    with contextlib.redirect_stdout(buffer):
        code = live.main(argv)
    return code, buffer.getvalue()


class MainTests(unittest.TestCase):
    def test_missing_authorization_flag_exits_one_without_files(self):
        argv = ['--requests', '1', '--concurrency', '1', '--max-tokens', '128']
        code, output = call_main(argv)
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(output)['posts_sent'], 0)
        self.assertEqual(json.loads(output)['failure'], 'explicit_authorization_flag_required')

    def test_passing_probe_exits_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            with mock.patch.object(live, 'capture_identity', mock.Mock(return_value=IDENTITY)), \
                    mock.patch.object(live, '_events',
                                      mock.Mock(return_value=(events_ok(TRACE), 5))), \
                    mock.patch.object(live, 'post_once',
                                      mock.Mock(return_value=(200, 'application/json', reply(), [TRACE]))):
                code, output = call_main(live_argv(directory))
            self.assertEqual(code, 0)
            assert_no_secret(self, output)
            out = json.loads((directory / 'out.json').read_text())
            self.assertTrue(out['passed'])
            self.assertEqual(out['posts_sent'], 1)
            assert_no_secret(self, out)

    def test_failing_probe_exits_nonzero_and_records_in_out(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            with mock.patch.object(live, 'capture_identity', mock.Mock(return_value=IDENTITY)), \
                    mock.patch.object(live, '_events',
                                      mock.Mock(return_value=(events_ok(TRACE), 5))), \
                    mock.patch.object(live, 'post_once',
                                      mock.Mock(return_value=(429, 'application/json', b'{}', []))):
                code, output = call_main(live_argv(directory))
            self.assertEqual(code, 2)
            self.assertNotEqual(code, 0)
            out = json.loads((directory / 'out.json').read_text())
            self.assertFalse(out['passed'])
            self.assertEqual(out['posts_sent'], 1)
            assert_no_secret(self, out)

    def test_initial_identity_failure_exits_one_with_confident_zero(self):
        # pid/epoch/exe/config/library/listener/status 任一不符都必须在首个 POST 前拒绝。
        codes = ['identity_mismatch:' + field for field in (
            'pid', 'startticks_epoch', 'core_sha256', 'lib_sha256', 'config_sha256',
            'plugin_version', 'management_boundary')] + [
            'process_executable_path_changed', 'process_config_argument_missing',
            'process_config_path_changed', 'expected_library_not_mapped_or_deleted',
            'mapped_library_file_identity_changed',
            'gateway_listener_not_owned_by_expected_cpa', 'management_read_failed']
        for code in codes:
            with self.subTest(code=code), tempfile.TemporaryDirectory() as directory:
                directory = Path(directory)
                broken = mock.Mock(side_effect=ValueError(code))
                wire = mock.Mock(return_value=(200, 'application/json', reply(), [TRACE]))
                with mock.patch.object(live, 'capture_identity', broken), \
                        mock.patch.object(live, 'post_once', wire):
                    exit_code, output = call_main(live_argv(directory))
                self.assertEqual(exit_code, 1)
                self.assertEqual(wire.call_count, 0)
                self.assertEqual(json.loads(output)['posts_sent'], 0)
                out = json.loads((directory / 'out.json').read_text())
                self.assertEqual(out['posts_sent'], 0)
                self.assertEqual(out['failure'], 'initial_identity_preflight_failed:' + code)

    def test_unrecoverable_failure_reports_none_not_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            with mock.patch.object(live, 'run_probe',
                                   mock.Mock(side_effect=RuntimeError('public-secret-marker'))):
                code, output = call_main(live_argv(directory))
            self.assertEqual(code, 1)
            out = json.loads((directory / 'out.json').read_text())
            self.assertIsNone(out['posts_sent'])
            self.assertEqual(out['failure'], 'RuntimeError')
            assert_no_secret(self, out)


if __name__ == '__main__':
    unittest.main(verbosity=2)
