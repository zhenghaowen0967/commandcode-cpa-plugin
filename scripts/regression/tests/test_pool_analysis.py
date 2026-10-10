"""账号池事件重建与整批归属证明的离线回归（公开合成夹具）。"""
import sys
import unittest
from pathlib import Path

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import fixtures  # noqa: E402
from pool_analysis import MODE_HOST, derive_mode, prove_coverage  # noqa: E402
from pool_analysis import observe_pool, response_statistics  # noqa: E402


def analyze(cases, observations, mode=None, prior=10):
    m = mode or derive_mode(cases, observations['events'])
    host_ids = {row.get('host_request_id') for row in cases} - {None}
    pool = observe_pool(observations['events'],
                        mode='test' if m == MODE_HOST else 'all', host_ids=host_ids)
    return pool, prove_coverage(cases, observations, prior, pool, mode=m)


class ObservationTests(unittest.TestCase):
    def test_anonymous_events_are_observed_but_not_attributed(self):
        observations = fixtures.observations()
        observations['events'] = [
            {'sequence': 1, 'request_id': 'public-external', 'attempt_id': 'x',
             'action': 'acquire', 'reason': 'acquired', 'account_ordinal': 3},
            {'sequence': 2, 'request_id': 'public-external', 'attempt_id': 'x',
             'action': 'settle', 'reason': 'upstream_complete', 'account_ordinal': 3},
        ]
        result = observe_pool(observations['events'], mode='all')
        self.assertFalse(result['host_request_id_attribution_proven'])
        self.assertEqual(result['scope'], 'all_observed_pool_requests_not_attributed_to_tests')
        self.assertEqual(result['remaining_new_owners'], 0)
        self.assertEqual(result['group_peaks'], {3: 1})

    def test_complete_set_proves_batch_and_never_claims_pairing(self):
        cases = fixtures.cases()
        pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
        self.assertTrue(proof['whole_test_lease_set_proven'])
        self.assertEqual(proof['new_acquire_count'], 2)
        self.assertEqual(proof['distinct_client_trace_count'], 2)
        self.assertEqual(proof['distinct_host_request_count'], 2)
        self.assertEqual(proof['global_sequence_span'], 6)
        self.assertEqual(proof['errors'], [])

    def test_missing_host_id_falls_back_to_legacy_collection_mode(self):
        # 删掉**所有** host id 后走 legacy：集合证明仍成立，但不声称逐条连接。
        cases = fixtures.legacy_cases()
        self.assertFalse(any('host_request_id' in row for row in cases))
        observations = fixtures.legacy_observations()
        pool, proof = analyze(cases, observations)
        self.assertEqual(proof['mode'], 'legacy')
        self.assertFalse(proof['per_request_host_trace_join_present'])
        # 客户端 trace（USAGE_*）不得被当成 host 事件 ID。
        self.assertFalse({fixtures.USAGE_A, fixtures.USAGE_B} &
                         {e['request_id'] for e in observations['events']})

    def test_legacy_collection_evidence_needs_no_host_id(self):
        # 旧 0.1.7 证据没有 host_request_id，仍能完成集合级等量与守恒证明。
        cases = fixtures.legacy_cases()
        observations = fixtures.legacy_observations()
        pool, proof = analyze(cases, observations)  # 自动判定 legacy
        self.assertEqual(proof['mode'], 'legacy')
        self.assertTrue(proof['whole_test_lease_set_proven'])
        self.assertEqual(proof['new_acquire_count'], 2)
        self.assertFalse(proof['per_request_host_trace_join_present'])
        # usage request_id 不得被当成 host id：事件里根本没有这些 UUID。
        event_ids = {e['request_id'] for e in observations['events']}
        self.assertFalse({fixtures.USAGE_A, fixtures.USAGE_B} & event_ids)

    def test_legacy_cap_rejection_plus_final_success_is_supported(self):
        cases = fixtures.legacy_cases_with_cap_rejection()
        pool, proof = analyze(cases, fixtures.legacy_observations())
        self.assertTrue(proof['whole_test_lease_set_proven'])
        self.assertEqual(proof['successful_request_count'], 2)

    def test_legacy_unexplained_failure_cannot_pass(self):
        cases = fixtures.legacy_cases()
        cases[0]['cpa_usage'].append(
            {'request_id': fixtures.USAGE_A, 'failed': 1, 'fail_status_code': 500,
             'latency_ms': 10})
        pool, proof = analyze(cases, fixtures.legacy_observations())
        self.assertIn('legacy_unexplained_usage_failure_attempt', proof['errors'])

    def test_legacy_two_final_successes_cannot_pass(self):
        cases = fixtures.legacy_cases()
        cases[0]['cpa_usage'] = cases[0]['cpa_usage'] + cases[1]['cpa_usage']
        pool, proof = analyze(cases, fixtures.legacy_observations())
        self.assertIn('legacy_usage_final_success_not_unique', proof['errors'])

    def test_legacy_requires_exact_session(self):
        cases = fixtures.legacy_cases()
        cases[0]['association'] = 'time_overlap_guess'
        pool, proof = analyze(cases, fixtures.legacy_observations())
        self.assertIn('legacy_association_not_exact_session', proof['errors'])

    def test_legacy_with_client_traces_derives_host_from_events(self):
        # 给了逐条 trace 时，必须用事件反查 host，而不是只信 callercase。
        cases = fixtures.legacy_cases()
        cases[0]['client_trace_id'] = fixtures.TRACE_A
        cases[1]['client_trace_id'] = fixtures.TRACE_B
        pool, proof = analyze(cases, fixtures.observations())
        self.assertTrue(proof['whole_test_lease_set_proven'])
        self.assertTrue(proof['per_request_host_trace_join_present'])

    def test_event_trace_mismatch_is_rejected(self):
        # cases 说 HOST_A↔TRACE_A，但事件里 pick 的 trace 被换成别的：owner 事件不一致即失败。
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['events'][0]['trace_id'] = '99999999-9999-4999-8999-999999999999'
        observations['events'][2]['trace_id'] = '99999999-9999-4999-8999-999999999999'
        pool, proof = analyze(cases, observations)
        self.assertFalse(proof['per_request_host_trace_join_present'])
        self.assertIn('event_owner_events_trace_inconsistent', proof['errors'])
        self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_pick_trace_without_acquire_settle_trace_cannot_fake_join(self):
        # 缺陷6：host 的 pick 有 trace，但 acquire/settle 无 trace —— 不能算已连接。
        cases = fixtures.cases()
        observations = fixtures.observations()
        for event in observations['events']:
            if event['action'] in ('acquire', 'settle'):
                event['trace_id'] = None
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertFalse(proof['per_request_host_trace_join_present'])
        self.assertIn('event_owner_events_trace_inconsistent', proof['errors'])
        self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_shared_parent_trace_is_allowed_but_not_claimed_unique(self):
        # typed 合同允许 1 trace→N host；必须报告多个 host 且不声称唯一配对，
        # 但不能把合法共享 trace 当成整体集合失败。
        cases = fixtures.legacy_cases()
        cases[0]['client_trace_id'] = fixtures.TRACE_REUSE
        cases[1]['client_trace_id'] = fixtures.TRACE_REUSE
        observations = fixtures.observations()
        for event in observations['events']:
            event['trace_id'] = fixtures.TRACE_REUSE
        pool, proof = analyze(cases, observations)
        self.assertTrue(proof['per_request_host_trace_join_present'])
        self.assertTrue(proof['whole_test_lease_set_proven'])
        self.assertEqual(proof['verified_host_request_count'], 2)
        for link in proof['trace_links']:
            self.assertFalse(link['unique_pairing'])
            self.assertEqual(link['host_request_ids'], [fixtures.HOST_A, fixtures.HOST_B])

    def test_duplicate_host_mapping_does_not_count_two_requests(self):
        cases = fixtures.cases()
        cases[1]['host_request_id'] = cases[0]['host_request_id']
        pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
        self.assertIn('host_ids_not_unique', proof['errors'])
        self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_trace_id_cannot_pose_as_host_id(self):
        cases = fixtures.cases()
        cases[0]['host_request_id'] = cases[0]['client_trace_id']
        pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
        self.assertIn('trace_id_must_not_pose_as_host_id', proof['errors'])

    def test_incomplete_per_request_trace_cannot_claim_join(self):
        cases = fixtures.cases()
        del cases[0]['client_trace_id']
        pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
        self.assertFalse(proof['whole_test_lease_set_proven'])
        self.assertIn('per_request_trace_incomplete', proof['errors'])
        self.assertFalse(proof['per_request_host_trace_join_present'])

    def test_external_acquire_prevents_batch_attribution(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        extra = dict(observations['events'][1], sequence=17, request_id='external',
                     attempt_id='e1')
        observations['events'].append(extra)
        observations['events'].append(dict(extra, sequence=18, action='settle'))
        observations['event_completeness_readback'].update(
            last_test_event_sequence=18, period_request_events=8)
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertFalse(proof['whole_test_lease_set_proven'])
        self.assertIn('new_lease_set_not_equal_to_success_set_size', proof['errors'])

    def test_missing_capture_cannot_pass(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['events'] = [e for e in observations['events']
                                  if e['request_id'] != fixtures.HOST_B]
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('new_lease_set_not_equal_to_success_set_size', proof['errors'])

    def test_missing_runtime_readback_cannot_pass(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations.pop('event_completeness_readback')
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('runtime_event_buffer_completeness_not_confirmed', proof['errors'])

    def test_oldest_retained_sequence_must_cover_start(self):
        # 丢历史起点（最老保留序号越过初始游标）仍能绿是缺陷；此处必须失败。
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['event_completeness_readback']['oldest_retained_request_sequence'] = 11
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('runtime_event_buffer_retention_boundary_failed', proof['errors'])

    def test_empty_boundary_accounts_cannot_pass(self):
        # 首末采样账号为空列表时 all([]) 会假过；必须失败。
        for index, name in ((0, 'start'), (-1, 'end')):
            cases = fixtures.cases()
            observations = fixtures.observations()
            observations['samples'][index]['accounts'] = []
            pool, proof = analyze(cases, observations, mode=MODE_HOST)
            self.assertFalse(proof['whole_test_lease_set_proven'],
                             'empty boundary %s must fail' % name)

    def test_boundary_account_shape_change_cannot_pass(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['samples'][-1]['accounts'].append(
            {'account_ordinal': 2, 'inflight': 0, 'cap': 10, 'status': 'eligible'})
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('snapshot_account_shape_changed', proof['errors'])

    def test_negative_or_nonpositive_event_ordinal_is_flagged(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        # 把一个 acquire 改成 -1：非正整数被拒绝，acquire 集合不再等于成功集合。
        observations['events'][1]['account_ordinal'] = -1
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('account_conservation_not_met', proof['errors'])
        self.assertIn('duplicate_or_unmapped_acquire', pool['errors'])

    def test_event_trace_inconsistency_rejected_in_host_mode(self):
        # host 模式也必须核对事件 trace 与派生 host 一致。
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['events'][1]['request_id'] = 'host-cccc-9999'  # acquire 换到别的 host
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertTrue(any(e in ('event_host_ids_do_not_match_cases',
                                  'new_lease_set_not_equal_to_success_set_size',
                                  'event_owner_events_trace_inconsistent')
                            for e in proof['errors']), proof['errors'])

    def test_ring_overflow_and_offset_cursor_cannot_pass(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['events'][-1]['sequence'] = 510
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('event_ring_may_have_wrapped', proof['errors'])
        observations = fixtures.observations()
        pool, proof = analyze(cases, observations, prior=12)
        self.assertIn('event_cursor_not_advanced', proof['errors'])

    def test_boundary_pool_must_start_and_end_empty(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['samples'][0]['accounts'][0]['inflight'] = 1
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('boundary_pool_not_empty_at_start', proof['errors'])
        observations = fixtures.observations()
        observations['samples'][-1]['accounts'][0]['inflight'] = 1
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('boundary_pool_not_empty_at_end', proof['errors'])

    def test_account_distribution_mismatch_cannot_pass(self):
        cases = fixtures.cases()
        cases[0]['server_execution']['account_ordinal'] = 2
        pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
        self.assertIn('account_conservation_not_met', proof['errors'])

    def test_cap_breach_is_evidence_gap(self):
        # 观察到的 cap 高于共享硬 cap10：采样契约失败，集合不能通过。
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['samples'][0]['accounts'][0]['cap'] = 11
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('snapshot_account_cap_not_shared', proof['errors'])
        self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_unverified_or_http_error_cannot_pass(self):
        for field, value in (('attempt_status', 'response_received'), ('http', 500)):
            cases = fixtures.cases()
            cases[0][field] = value
            pool, proof = analyze(cases, fixtures.observations(), mode=MODE_HOST)
            self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_nonnatural_settle_is_flagged(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        observations['events'][-1]['reason'] = 'upstream_canceled'
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertIn('nonnatural_settle_present', proof['errors'])
        self.assertEqual(pool['natural_settle_count'], 1)

    def test_local_cap_rejection_is_not_an_owner_or_failure(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        # 本地 cap 拒绝不带 trace（它不是 owner 关键事件），但不得破坏整体集合。
        rejection = dict(observations['events'][1], sequence=17, action='acquire_rejected',
                         attempt_id=None, reason='concurrency_limit', trace_id=None)
        observations['events'].append(rejection)
        observations['event_completeness_readback'].update(
            last_test_event_sequence=17, period_request_events=7)
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertEqual(len(pool['local_cap_rejections']), 1)
        self.assertEqual(pool['errors'], [])
        self.assertTrue(proof['whole_test_lease_set_proven'], proof['errors'])
        rejection['reason'] = 'unexplained'
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertFalse(proof['whole_test_lease_set_proven'])

    def test_same_host_cap_retry_proves_busy_switch(self):
        def event(seq, action, request, group, reason='acquired', candidates=None):
            return {'sequence': seq, 'request_id': request, 'attempt_id': request + '-a',
                    'account_ordinal': group, 'action': action, 'model': fixtures.MODEL,
                    'reason': reason, 'candidates': candidates or []}
        high = {'account_ordinal': 1, 'headroom': 0.9, 'remaining_credits': 60,
                'inflight': 9, 'limit': 10, 'eligible': True, 'reason': 'eligible'}
        low = dict(high, account_ordinal=3, headroom=0.2, remaining_credits=40, inflight=0)
        events = [event(1, 'pick', 'retry', 1, 'selected', [high, low])]
        events += [event(i + 2, 'acquire', 'r%s' % i, 1) for i in range(10)]
        events += [event(12, 'acquire_rejected', 'retry', 1, 'concurrency_limit'),
                   event(13, 'pick', 'retry', 3, 'selected', [low])]
        self.assertEqual(observe_pool(events, mode='all')['busy_switch_events'], [])
        events.append(event(14, 'acquire', 'retry', 3))
        result = observe_pool(events, mode='all')
        self.assertEqual(result['total_peak'], 11)
        self.assertEqual(len(result['busy_switch_events']), 1)
        self.assertEqual(result['busy_switch_events'][0]['confirmed_acquire_sequence'], 14)
        self.assertEqual(result['busy_switch_events'][0]['full_inflight'], 10)
        events[-2]['request_id'] = 'different-host'
        self.assertEqual(observe_pool(events, mode='all')['busy_switch_events'], [])

    def test_statistics_measure_complete_response_not_first_token(self):
        result = response_statistics([
            {'elapsed_seconds': 2, 'http': 200, 'attempt_status': 'verified', 'ttft_ms': 100},
            {'elapsed_seconds': 4, 'http': 200, 'attempt_status': 'verified', 'ttft_ms': 200}])
        self.assertEqual(result['complete_response_seconds'],
                         {'min': 2, 'median': 3.0, 'p95': 4, 'max': 4})
        self.assertNotIn('ttft_ms', result)

    def test_string_ordinals_normalize_without_false_mismatch(self):
        cases = fixtures.cases()
        observations = fixtures.observations()
        for event in observations['events']:
            event['account_ordinal'] = '1'  # JSON 往返可能把 int 变成 str
        pool, proof = analyze(cases, observations, mode=MODE_HOST)
        self.assertEqual(pool['group_peaks'], {1: 2})
        self.assertTrue(proof['whole_test_lease_set_proven'])

    def test_lease_peak_above_cap_is_flagged(self):
        events = [{'sequence': i + 1, 'request_id': 'r%s' % i, 'attempt_id': 'a%s' % i,
                   'action': 'acquire', 'reason': 'acquired', 'account_ordinal': 1,
                   'model': fixtures.MODEL} for i in range(11)]
        result = observe_pool(events, mode='all')
        self.assertEqual(result['group_peaks'], {1: 11})
        self.assertIn('observed_group_cap_exceeded', result['errors'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
