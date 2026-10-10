"""analyze 子命令的离线端到端回归：读明确JSON证据、拒绝覆盖、trace连接。"""
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import analyze  # noqa: E402
import fixtures  # noqa: E402


class AnalyzeConversionTests(unittest.TestCase):
    def test_trace_id_connects_client_trace_to_host_request_id(self):
        link = analyze.connect_trace(fixtures.cases(), fixtures.TRACE_A,
                                     events=fixtures.observations()['events'])
        self.assertEqual(link['host_request_ids'], [fixtures.HOST_A])
        self.assertTrue(link['unique_pairing'])
        self.assertTrue(link['joined'])
        self.assertEqual(link['join_source'], 'event_trace')
        self.assertEqual(link['trace_id'], fixtures.TRACE_A)
        self.assertFalse(link['host_request_id_is_client_fabricable'])

    def test_connect_trace_without_events_is_unverified_hint_not_proof(self):
        # 没有事件时不提供证明能力，只回未核验提示，绝不给人可信错觉。
        link = analyze.connect_trace(fixtures.cases(), fixtures.TRACE_A)
        self.assertFalse(link['joined'])
        self.assertEqual(link['join_source'], 'unverified_hint')
        self.assertEqual(link['host_request_ids'], [])

    def test_shared_parent_trace_reports_all_hosts(self):
        cases = fixtures.legacy_cases()
        cases[0]['client_trace_id'] = fixtures.TRACE_REUSE
        cases[1]['client_trace_id'] = fixtures.TRACE_REUSE
        events = fixtures.observations()['events']
        for event in events:
            event['trace_id'] = fixtures.TRACE_REUSE
        link = analyze.connect_trace(cases, fixtures.TRACE_REUSE, events=events)
        self.assertFalse(link['unique_pairing'])
        self.assertEqual(link['host_request_ids'], [fixtures.HOST_A, fixtures.HOST_B])
        self.assertTrue(link['joined'])

    def test_trace_id_must_be_standard_uuid_and_match_once(self):
        with self.assertRaises(ValueError):
            analyze.connect_trace(fixtures.cases(), 'not-a-uuid')
        with self.assertRaises(ValueError):
            analyze.connect_trace(fixtures.cases(), '8b7c1e22-0000-4000-8000-000000000000')

    def test_non_uuid_trace_is_rejected_never_used_as_host_id(self):
        cases = fixtures.cases()
        cases[0]['client_trace_id'] = 'usage-id-that-is-not-a-uuid'
        with self.assertRaises(ValueError):
            analyze.build_report(cases, fixtures.observations(), 10)

    def test_legacy_evidence_without_per_request_trace_stays_collection_level(self):
        # 旧 0.1.7 证据没有逐条 trace：集合级证明仍成立，但不声称逐条连接。
        cases = fixtures.cases()
        for row in cases:
            del row['client_trace_id']
        report = analyze.build_report(cases, fixtures.observations(), 10)
        self.assertTrue(report['passed'])
        self.assertTrue(report['coverage_proof']['whole_test_lease_set_proven'])
        self.assertFalse(report['coverage_proof']['per_request_host_trace_join_present'])
        self.assertFalse(report['budget']['per_request_trace_present'])
        self.assertIsNone(report['trace_link'])

    def test_build_report_flags_baseline_mismatch(self):
        cases = fixtures.cases()
        baseline = fixtures.identity()
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      baseline=baseline, trace_id=fixtures.TRACE_A)
        self.assertTrue(report['passed'])
        self.assertEqual(report['inputs']['baseline_mismatched_fields'], [])
        link = report['trace_link']
        self.assertEqual(link['host_request_ids'], [fixtures.HOST_A])
        self.assertTrue(link['joined'])

        drifted = fixtures.identity(pid=9999)
        report = analyze.build_report(cases, fixtures.observations(), 10, baseline=drifted)
        self.assertFalse(report['passed'])
        self.assertIn('case_0:pid', report['inputs']['baseline_mismatched_fields'])
        self.assertIn('case_1:pid', report['inputs']['baseline_mismatched_fields'])

    def test_missing_evidence_fields_fail_loud(self):
        cases = fixtures.cases()
        del cases[0]['server_execution']
        with self.assertRaises(ValueError):
            analyze.build_report(cases, fixtures.observations(), 10)
        with self.assertRaises(ValueError):
            # 空请求集合是证据缺失，而不是“零请求通过”。
            analyze.build_report([], fixtures.observations(), 10)
        # observations 缺少 events/samples 时要在证明阶段明确失败，而不是静默通过。
        report = analyze.build_report(fixtures.cases(), {'events': [], 'samples': []}, 10)
        self.assertFalse(report['passed'])
        self.assertIn('missing_events', report['coverage_proof']['errors'])

    def test_collection_proof_alone_is_not_capacity_pass(self):
        # 17 个集合证明不等于峰值 11 的容量验收：未声明门时容量通过必须为 false。
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10)
        self.assertTrue(report['collection_proven'])
        self.assertFalse(report['capacity_acceptance_passed'])
        self.assertFalse(report['capacity_contract']['declared_capacity_gate'])
        self.assertTrue(report['capacity_contract']['checks']['collection_pass_is_not_capacity_pass'])

    def test_overall_passed_includes_declared_capacity_gate(self):
        # 显式 expected_peak 不达：collection_proven 仍真，但 overall passed 必须 false。
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      analysis_contract=analyze.AnalysisContract(expected_peak=99))
        self.assertTrue(report['collection_proven'])
        self.assertFalse(report['passed'])
        self.assertFalse(report['capacity_acceptance_passed'])

    def test_overall_passed_includes_budget_failure(self):
        # 声明 max_posts 少于实测 attempt：overall passed 必须 false。
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      analysis_contract=analyze.AnalysisContract(max_posts=1))
        self.assertFalse(report['budget']['posts_allowed'])
        self.assertFalse(report['passed'])
        self.assertFalse(report['capacity_acceptance_passed'])

    def test_declared_max_tokens_requires_present_budget(self):
        cases = fixtures.cases()  # 没有 max_tokens 字段
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      analysis_contract=analyze.AnalysisContract(max_tokens=512))
        self.assertFalse(report['budget']['max_tokens_allowed'])
        self.assertFalse(report['passed'])
        for bad in (True, -1, 0, '256'):
            cases = fixtures.cases()
            for row in cases:
                row['max_tokens'] = bad
            report = analyze.build_report(cases, fixtures.observations(), 10,
                                          analysis_contract=analyze.AnalysisContract(max_tokens=512))
            self.assertFalse(report['budget']['max_tokens_allowed'],
                             'bad max_tokens %r must fail' % bad)
        good = fixtures.cases()
        for row in good:
            row['max_tokens'] = 128
        report = analyze.build_report(good, fixtures.observations(), 10,
                                      analysis_contract=analyze.AnalysisContract(max_tokens=512))
        self.assertTrue(report['budget']['max_tokens_allowed'])

    def test_non_first_case_identity_drift_is_flagged(self):
        cases = fixtures.cases()
        cases[1]['identity'] = fixtures.identity(pid=9999)
        report = analyze.build_report(cases, fixtures.observations(), 10)
        self.assertIn(1, report['inputs']['baseline_mismatched_fields'])
        self.assertFalse(report['passed'])
        self.assertIn('identity_baseline_mismatch',
                      report['capacity_contract']['reasons'])

    def test_capacity_acceptance_blocked_by_baseline_mismatch(self):
        contract = analyze.AnalysisContract(expected_peak=2)
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      baseline=fixtures.identity(pid=1),
                                      analysis_contract=contract)
        self.assertFalse(report['capacity_acceptance_passed'])

    def test_declared_peak_gate_decides_capacity(self):
        met = analyze.AnalysisContract(expected_peak=2)
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      analysis_contract=met)
        self.assertTrue(report['capacity_acceptance_passed'])
        unmet = analyze.AnalysisContract(expected_peak=11)
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      analysis_contract=unmet)
        self.assertFalse(report['capacity_acceptance_passed'])
        self.assertTrue(any('observed_peak_below_expected' in r
                            for r in report['capacity_contract']['reasons']))

    def test_required_busy_switch_absent_blocks_capacity(self):
        contract = analyze.AnalysisContract(expected_peak=2, require_busy_switch=True)
        report = analyze.build_report(fixtures.cases(), fixtures.observations(), 10,
                                      analysis_contract=contract)
        self.assertFalse(report['capacity_acceptance_passed'])
        self.assertIn('busy_switch_not_observed', report['capacity_contract']['reasons'])

    def test_declared_budget_is_enforced(self):
        contract = analyze.AnalysisContract(max_posts=1, max_tokens=64)
        cases = fixtures.cases()
        for row in cases:
            row['max_tokens'] = 256
        report = analyze.build_report(cases, fixtures.observations(), 10,
                                      analysis_contract=contract)
        self.assertFalse(report['budget']['posts_allowed'])
        self.assertFalse(report['budget']['max_tokens_allowed'])
        within = analyze.build_report(cases, fixtures.observations(), 10,
                                      analysis_contract=analyze.AnalysisContract(
                                          max_posts=5, max_tokens=512))
        self.assertTrue(within['budget']['posts_allowed'])
        self.assertTrue(within['budget']['max_tokens_allowed'])


class AnalyzeCliTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.cases_path = fixtures.write_json(self.dir / 'cases.json', fixtures.cases())
        self.observations_path = fixtures.write_json(self.dir / 'observations.json',
                                                     fixtures.observations())
        self.baseline_path = fixtures.write_json(self.dir / 'baseline.json', fixtures.identity())

    def tearDown(self):
        self.tmp.cleanup()

    def run_cli(self, extra=()):
        return subprocess.run(
            [sys.executable, str(REGRESSION_DIR / 'analyze.py'),
             '--cases', str(self.cases_path),
             '--observations', str(self.observations_path),
             '--prior-cursor', '10',
             '--baseline', str(self.baseline_path),
             '--trace-id', fixtures.TRACE_B,
             '--out', str(self.dir / 'report.json'), *extra],
            capture_output=True, text=True, timeout=60)

    def test_cli_writes_result_and_refuses_overwrite(self):
        out = self.dir / 'report.json'
        result = self.run_cli()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        report = json.loads(out.read_text(encoding='utf-8'))
        self.assertTrue(report['passed'])
        self.assertEqual(report['coverage_proof']['whole_test_lease_set_proven'], True)
        self.assertEqual(report['trace_link']['host_request_ids'], [fixtures.HOST_B])
        self.assertEqual(out.stat().st_mode & 0o777, 0o600)
        # 第二次运行必须拒绝覆盖，原报告保持不变。
        again = self.run_cli()
        self.assertEqual(again.returncode, 1)
        self.assertIn('output_exists_refusing_to_overwrite', again.stdout)
        self.assertEqual(json.loads(out.read_text(encoding='utf-8'))['passed'], True)

    def test_cli_fails_without_runtime_readback(self):
        self.observations_path.write_text(json.dumps(
            {k: v for k, v in fixtures.observations().items()
             if k != 'event_completeness_readback'}), encoding='utf-8')
        result = self.run_cli()
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)


if __name__ == '__main__':
    unittest.main(verbosity=2)
