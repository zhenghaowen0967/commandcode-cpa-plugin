"""分析器证据门的纯夹具反例（2026-10-10 主会话修复后补）。

只依赖公开合成夹具与 analyze 的公开入口，不含 mock、不联网、不读生产。
覆盖两处已修缺口的最小反例：
1. _measured_attempts 必须包含本地准入拒绝（acquire_rejected），limit 按 N+1 核。
2. 声明 baseline 时逐 case 校验 identity，缺失不得静默跳过。
3. 各 case 身份字段不一致时 capacity 与 overall 都必须失败。
"""
import sys
import unittest
from pathlib import Path

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import analyze  # noqa: E402
import fixtures  # noqa: E402


def local_cap_rejection(observations):
    """在一个合法事件流上并入一条本地 cap 拒绝（非 lease 生命周期事件、可无 trace）。"""
    rejection = dict(observations['events'][1], sequence=17, action='acquire_rejected',
                     attempt_id=None, reason='concurrency_limit', trace_id=None)
    observations['events'].append(rejection)
    observations['event_completeness_readback'].update(
        last_test_event_sequence=17, period_request_events=7)
    return observations


class MeasuredAttemptsTests(unittest.TestCase):
    def test_measured_attempts_includes_local_cap_rejections(self):
        # 直接锁公式：成功 Acquire + 本地准入拒绝。
        self.assertEqual(analyze._measured_attempts(
            {'acquire_count': 2, 'local_cap_rejections': [{'a': 1}]}), 3)
        self.assertEqual(analyze._measured_attempts(
            {'acquire_count': 2, 'local_cap_rejections': []}), 2)

    def test_rejection_plus_acquires_counts_one_more_and_limit_n_fails(self):
        # N=2 acquire + 1 本地拒绝 => measured=3；max_posts=2 必须失败，=3 才通过。
        cases = fixtures.cases()
        observations = local_cap_rejection(fixtures.observations())
        over = analyze.build_report(cases, observations, 10,
                                    analysis_contract=analyze.AnalysisContract(max_posts=2))
        self.assertTrue(over['collection_proven'], over['coverage_proof']['errors'])
        self.assertEqual(over['budget']['measured_attempt_count'], 3)
        self.assertFalse(over['budget']['posts_allowed'])
        self.assertFalse(over['passed'])
        within = analyze.build_report(cases, observations, 10,
                                      analysis_contract=analyze.AnalysisContract(max_posts=3))
        self.assertEqual(within['budget']['measured_attempt_count'], 3)
        self.assertTrue(within['budget']['posts_allowed'])
        self.assertTrue(within['passed'])


class BaselinePerCaseTests(unittest.TestCase):
    def test_late_case_missing_identity_fails_under_declared_baseline(self):
        cases = fixtures.cases()
        del cases[1]['identity']
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      baseline=fixtures.identity())
        self.assertIn('case_identity_missing:1', report['inputs']['baseline_mismatched_fields'])
        self.assertFalse(report['collection_proven'])
        self.assertFalse(report['passed'])

    def test_all_cases_missing_identity_fail_under_declared_baseline(self):
        cases = fixtures.cases()
        for row in cases:
            del row['identity']
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      baseline=fixtures.identity())
        self.assertIn('case_identity_missing:0', report['inputs']['baseline_mismatched_fields'])
        self.assertIn('case_identity_missing:1', report['inputs']['baseline_mismatched_fields'])
        self.assertFalse(report['passed'])

    def test_each_case_compared_not_only_first(self):
        # 首条匹配、次条漂移：两处都要有依据；次条必须被逐 case 比出。
        cases = fixtures.cases()
        cases[1]['identity'] = fixtures.identity(config_sha256='d' * 64)
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      baseline=fixtures.identity())
        self.assertIn('case_1:config_sha256',
                      report['inputs']['baseline_mismatched_fields'])
        self.assertNotIn('case_0:config_sha256',
                         report['inputs']['baseline_mismatched_fields'])


class PerCaseIdentityBlocksGatesTests(unittest.TestCase):
    def test_drifting_case_blocks_capacity_and_overall_even_when_peak_met(self):
        # 峰值门本应达成（expected_peak=2 且观测峰值=2），但某 case 身份漂移：
        # capacity 与 overall 都必须 false。
        cases = fixtures.cases()
        cases[1]['identity'] = fixtures.identity(lib_sha256='e' * 64)
        report = analyze.build_report(
            cases, fixtures.observations(), 10, baseline=fixtures.identity(),
            analysis_contract=analyze.AnalysisContract(expected_peak=2))
        self.assertEqual(report['capacity_contract']['checks']['peak'], 'met')
        self.assertIn('identity_baseline_mismatch',
                      report['capacity_contract']['reasons'])
        self.assertFalse(report['capacity_acceptance_passed'])
        self.assertFalse(report['passed'])


if __name__ == '__main__':
    unittest.main(verbosity=2)
