"""公共底座与回环传输的离线回归：指纹、原子journal、回环校验。"""
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import evidence  # noqa: E402
import fixtures  # noqa: E402
import live_wire  # noqa: E402


def message_response(text, tool=False, stop='end_turn'):
    content = [{'type': 'text', 'text': text}]
    if tool:
        content.append({'type': 'tool_use', 'name': 'public_tool'})
    return json.dumps({'type': 'message', 'content': content, 'stop_reason': stop,
                       'usage': {'input_tokens': 10, 'output_tokens': 8}}).encode()


class EvidenceTests(unittest.TestCase):
    def test_uuid_regex_accepts_standard_rejects_other(self):
        self.assertTrue(evidence.UUID_RE.match(fixtures.TRACE_A))
        self.assertFalse(evidence.UUID_RE.match('usage-01a12383'))
        self.assertFalse(evidence.UUID_RE.match(fixtures.TRACE_A + '-extra'))

    def test_fingerprint_normalizes_and_compares(self):
        fingerprint = evidence.fingerprint_identity(fixtures.identity())
        self.assertEqual(evidence.compare_identity(fingerprint, fixtures.identity()), [])
        self.assertEqual(evidence.compare_identity(fingerprint,
                         fixtures.identity(pid=1)), ['pid'])

    def test_atomic_write_new_refuses_overwrite_and_is_private(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'out.json'
            evidence.atomic_write_new(path, {'v': 1})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                evidence.atomic_write_new(path, {'v': 2})
            self.assertEqual(json.loads(path.read_text()), {'v': 1})

    def test_atomic_replace_is_private_and_leaves_no_temp(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'journal.json'
            evidence.atomic_replace_private(path, {'v': 1})
            evidence.atomic_replace_private(path, {'v': 2})
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(json.loads(path.read_text()), {'v': 2})
            self.assertEqual([p.name for p in Path(directory).iterdir()], ['journal.json'])

    def test_bounded_int_rejects_bool_and_out_of_range(self):
        self.assertEqual(evidence.bounded_int('n', 3, 1, 5), 3)
        with self.assertRaises(ValueError):
            evidence.bounded_int('n', True, 1, 5)
        with self.assertRaises(ValueError):
            evidence.bounded_int('n', 6, 1, 5)

    def test_require_loopback_rejects_remote_and_paths(self):
        self.assertEqual(evidence.require_loopback_url('http://127.0.0.1:15721'),
                         ('127.0.0.1', 15721))
        for bad in ('https://api.example.com', 'http://10.0.0.5:15721',
                    'http://127.0.0.1:15721/v1/messages', 'http://[::1]:9'):
            with self.assertRaises(ValueError):
                evidence.require_loopback_url(bad)

    def test_private_file_requires_0600_regular(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'key'
            path.write_text('public-test-token', encoding='utf-8')
            path.chmod(0o600)
            self.assertEqual(evidence.read_private_file(path), 'public-test-token')
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                evidence.read_private_file(path)
    def test_private_file_rejects_links_fifo_modes_and_controls(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'key'
            path.write_text('public-test-token\n')
            path.chmod(0o600)
            link = Path(directory) / 'linked-key'
            link.symlink_to(path)
            with self.assertRaises(OSError):
                evidence.read_private_file(link)
            fifo = Path(directory) / 'fifo'
            os.mkfifo(fifo, 0o600)
            with self.assertRaises(ValueError):
                evidence.read_private_file(fifo)
            for mode in (0o400, 0o700, 0o640):
                path.chmod(mode)
                with self.assertRaises(ValueError):
                    evidence.read_private_file(path)
            path.chmod(0o600)
            path.write_text('public-test-token\nX-Other: value')
            with self.assertRaises(ValueError):
                evidence.read_private_file(path)

    def test_private_file_checks_same_open_descriptor(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'key'
            path.write_text('public-test-token')
            path.chmod(0o600)
            actual = os.fstat
            with mock.patch.object(evidence.os, 'fstat', wraps=actual) as descriptor:
                self.assertEqual(evidence.read_private_file(path), 'public-test-token')
            self.assertEqual(descriptor.call_count, 1)

    def test_url_rejects_credentials_zero_port_and_queries(self):
        for url in ('http://public-key@127.0.0.1:10', 'http://127.0.0.1:0',
                    'http://127.0.0.1:10?q=1', 'http://127.0.0.1:10/#x'):
            with self.subTest(url=url), self.assertRaises(ValueError):
                evidence.require_loopback_url(url)


class WireTests(unittest.TestCase):
    def test_only_trace_uuids_retained_from_headers(self):
        response = mock.Mock()
        response.status = 200
        headers = {'Content-Type': 'application/json',
                   'x-cpa-trace-id': 'prefix:%s;public-secret-marker' % fixtures.TRACE_A,
                   'Set-Cookie': 'public-cookie-secret',
                   'Authorization': 'public-bearer-secret'}
        response.getheader.side_effect = lambda name, default=None: headers.get(name, default)
        response.read.return_value = b'{}'
        conn = mock.Mock()
        conn.getresponse.return_value = response
        with mock.patch.object(live_wire.http.client, 'HTTPConnection', return_value=conn):
            status, content_type, raw, trace = live_wire.post_once(
                '127.0.0.1', 15721, '/v1/messages', 'public-test-token', {})
        self.assertEqual((status, content_type, raw), (200, 'application/json', b'{}'))
        self.assertEqual(trace, [fixtures.TRACE_A])
        encoded = json.dumps(trace)
        for private in ('public-secret', 'public-cookie', 'public-bearer'):
            self.assertNotIn(private, encoded)
        conn.close.assert_called_once()

    def test_complete_capture_and_no_tool_request(self):
        decoded = live_wire.decode_message(message_response('请求追踪探针成功'))
        self.assertEqual(decoded['stop_reason'], 'end_turn')
        self.assertEqual(decoded['usage_summary'], {'input_tokens': 10, 'output_tokens': 8})
        self.assertEqual(decoded['new_tool_calls'], [])
        with self.assertRaises(ValueError):
            live_wire.decode_message(message_response('x', tool=True))
        with self.assertRaises(ValueError):
            live_wire.decode_message(message_response('x', stop='tool_use'))
        with self.assertRaises(ValueError):
            live_wire.decode_message(b'not json')

    def test_parent_trace_only_and_duplicate_header_is_ambiguous(self):
        response = mock.Mock()
        headers = {'x-request-id': fixtures.TRACE_A, 'x-trace-id': fixtures.TRACE_A,
                   'x-commandcode-pool-request-id': fixtures.TRACE_A}
        response.getheader.side_effect = lambda name, default='': headers.get(name, default)
        self.assertEqual(live_wire._extract_trace_ids(response), [])
        headers['x-cpa-trace-id'] = fixtures.TRACE_A + ',' + fixtures.TRACE_A
        self.assertEqual(live_wire._extract_trace_ids(response), [fixtures.TRACE_A] * 2)

    def test_bad_shapes_unknown_tools_and_token_truncation_fail(self):
        for value in ([], {'type': 'message', 'content': 'not-blocks'},
                      {'type': 'message', 'content': [None]},
                      {'type': 'message', 'content': [{'type': 'server_tool_use'}]},
                      {'type': 'message', 'content': [{'type': 'text', 'text': 1}]}):
            with self.subTest(value=value), self.assertRaises(ValueError):
                live_wire.decode_message(json.dumps(value).encode())
        with self.assertRaises(ValueError):
            live_wire.decode_message(message_response('请求追踪探针成功', stop='max_tokens'))
        value = json.loads(message_response('请求追踪探针成功'))
        value['usage']['output_tokens'] = 'public-secret-marker'
        with self.assertRaises(ValueError):
            live_wire.decode_message(json.dumps(value).encode())

    def test_oversized_response_is_not_a_success(self):
        response = mock.Mock()
        response.status = 200
        response.getheader.return_value = ''
        response.read.return_value = b'x' * (evidence.MAX_RESPONSE_BYTES + 1)
        conn = mock.Mock()
        conn.getresponse.return_value = response
        with mock.patch.object(live_wire.http.client, 'HTTPConnection', return_value=conn):
            with self.assertRaises(ValueError):
                live_wire.post_once('127.0.0.1', 15721, '/v1/messages', 'public-token', {})


if __name__ == '__main__':
    unittest.main(verbosity=2)
