"""离线审计反例只调用钩子，不实际联网或读取被禁止的路径。"""
import unittest
from unittest import mock

import offline


class OfflineGuardTests(unittest.TestCase):
    def test_connect_is_blocked_before_transport(self):
        with mock.patch.object(offline, 'VIOLATIONS', []):
            with self.assertRaises(PermissionError):
                offline._audit('socket.connect', (None, ('127.0.0.1', 9)))
            self.assertEqual(offline.VIOLATIONS, [('socket.connect', 'blocked')])

    def test_marked_read_is_blocked_without_recording_contents(self):
        with mock.patch.object(offline, 'VIOLATIONS', []):
            with self.assertRaises(PermissionError):
                offline._audit('open', ('/proc/synthetic/stat', 'r', 0))
            self.assertEqual(offline.VIOLATIONS, [('open', 'blocked')])

    def test_public_fixture_and_unrelated_events_are_allowed(self):
        with mock.patch.object(offline, 'VIOLATIONS', []):
            offline._audit('open', ('/tmp/public-fixture.json', 'r', 0))
            offline._audit('unrelated.event', ())
            self.assertEqual(offline.VIOLATIONS, [])


if __name__ == '__main__':
    unittest.main()
