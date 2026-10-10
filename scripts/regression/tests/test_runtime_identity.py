"""runtime_identity 的离线回归：身份基线、真实进程读取、maps 文件身份、同源管理端口。

所有 /proc 读取都在低层用 mock 替换（Path.read_text/read_bytes/stat、os.readlink、
_digest、owns_listener），本测试不访问真实 /proc、不读取任何真实凭据、不联网。
"""
import contextlib
import hashlib
import json
import os
import stat
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

REGRESSION_DIR = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REGRESSION_DIR))

import runtime_identity  # noqa: E402

GATEWAY = 'http://127.0.0.1:15721'
BOUNDARY = 'http://127.0.0.1:15721/v0/management/plugins/commandcode-pool'
CORE = '/opt/cpa/commandcode-pool-next'
LIBRARY = '/opt/cpa/lib/commandcode-pool-next.so'
CONFIG = '/opt/cpa/config.yaml'
CWD = '/opt/cpa'
PID = 4321
STARTTICKS = 98765


def baseline(**overrides):
    base = {
        'pid': PID,
        'startticks_epoch': '%d:%d' % (PID, STARTTICKS),
        'core_sha256': 'a' * 64,
        'lib_sha256': 'b' * 64,
        'config_sha256': 'c' * 64,
        'plugin_version': '0.1.8-local',
        'management_boundary': BOUNDARY,
        'core_path': CORE,
        'library_path': LIBRARY,
        'config_path': CONFIG,
        'identity_confirmed': True,
    }
    base.update(overrides)
    return base


def stat_text(pid=PID, startticks=STARTTICKS, comm='commandcode pool-next'):
    # stat 第 22 字段是 starttime；进程名可含空格/括号，只用最后一个 ')' 切分。
    after = ' '.join(['R'] + [str(k) for k in range(1, 19)] + [str(startticks)])
    return '%d (%s) %s' % (pid, comm, after)


def maps_line(inode=12345, dev=(8, 1), path=LIBRARY):
    return '7f000000-7f001000 r-xp 00000000 %x:%x %d %s' % (dev[0], dev[1], inode, path)


class Proc:
    """一份合成的 /proc 视图；默认全部与 baseline 一致（应当通过）。"""

    def __init__(self, **overrides):
        self.exe = CORE
        self.cwd = CWD
        self.cmdline = ('%s\x00--config\x00%s\x00' % (CORE, CONFIG)).encode()
        self.maps = maps_line()
        self.stat = stat_text()
        self.digest = {CORE: 'a' * 64, LIBRARY: 'b' * 64, CONFIG: 'c' * 64}
        self.ino = 12345
        self.dev = os.makedev(8, 1)
        self.owned = True
        self.__dict__.update(overrides)


@contextlib.contextmanager
def fake_proc(proc, management=None):
    def read_text(self, *args, **kwargs):
        name = str(self)
        if name.endswith('/maps'):
            return proc.maps
        if name.endswith('/stat'):
            return proc.stat
        if name.endswith('net/tcp'):
            return ''
        raise FileNotFoundError(name)

    def read_bytes(self, *args, **kwargs):
        if str(self).endswith('/cmdline'):
            return proc.cmdline
        raise FileNotFoundError(str(self))

    def readlink(path):
        name = str(path)
        if name.endswith('/exe'):
            return proc.exe
        if name.endswith('/cwd'):
            return proc.cwd
        raise FileNotFoundError(name)

    def stat_fn(self, *args, **kwargs):
        if str(self) == LIBRARY:
            return types.SimpleNamespace(st_ino=proc.ino, st_dev=proc.dev,
                                         st_mode=stat.S_IFREG)
        raise FileNotFoundError(str(self))

    def digest(path):
        name = str(path)
        if name.endswith('/exe'):
            return proc.digest[CORE]
        return proc.digest.get(name, 'x' * 64)

    status = management if management is not None else {'status': {'plugin_version': '0.1.8-local'}}
    with mock.patch.object(runtime_identity.Path, 'resolve', lambda self, strict=True: self), \
            mock.patch.object(runtime_identity.Path, 'read_text', read_text), \
            mock.patch.object(runtime_identity.Path, 'read_bytes', read_bytes), \
            mock.patch.object(runtime_identity.Path, 'stat', stat_fn), \
            mock.patch.object(runtime_identity.os, 'readlink', readlink), \
            mock.patch.object(runtime_identity, '_digest', digest), \
            mock.patch.object(runtime_identity, 'owns_listener', return_value=proc.owned), \
            mock.patch.object(runtime_identity, 'management_get', return_value=status):
        yield


def assert_no_secret(case, value):
    encoded = json.dumps(value, ensure_ascii=False)
    for secret in ('public-secret-marker', 'public-mgmt', 'public-management-token'):
        case.assertNotIn(secret, encoded)


class BaselineTests(unittest.TestCase):
    def test_accepts_confirmed_loopback_baseline(self):
        host, port, path = runtime_identity.validate_baseline(baseline(), GATEWAY)
        self.assertEqual((host, port), ('127.0.0.1', 15721))
        self.assertEqual(path, '/v0/management/plugins/commandcode-pool')

    def test_rejects_unconfirmed_and_bad_pid(self):
        for bad in (baseline(identity_confirmed=False), baseline(identity_confirmed='yes')):
            with self.subTest(bad=bad), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(bad, GATEWAY)
            self.assertEqual(str(caught.exception), 'identity_not_confirmed')
        for bad in (baseline(pid=True), baseline(pid=0), baseline(pid=-1), baseline(pid='1234')):
            with self.subTest(pid=bad['pid']), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(bad, GATEWAY)
            self.assertEqual(str(caught.exception), 'baseline_pid_invalid')

    def test_every_identity_and_path_field_required(self):
        # pid 有自己的取值校验（空/非正数先判 baseline_pid_invalid），其余字段走缺失校验。
        for field in [f for f in runtime_identity.IDENTITY_FIELDS + runtime_identity.PATH_FIELDS
                      if f != 'pid']:
            with self.subTest(field=field), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(baseline(**{field: ''}), GATEWAY)
            self.assertEqual(str(caught.exception), 'baseline_field_missing:' + field)

    def test_digest_epoch_path_version_are_validated(self):
        for field in ('core_sha256', 'lib_sha256', 'config_sha256'):
            bad = baseline(**{field: 'Z' * 64})
            with self.subTest(field=field), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(bad, GATEWAY)
            self.assertEqual(str(caught.exception), 'baseline_digest_invalid:' + field)
        with self.assertRaises(ValueError) as caught:
            runtime_identity.validate_baseline(baseline(startticks_epoch='wrong'), GATEWAY)
        self.assertEqual(str(caught.exception), 'baseline_epoch_invalid')
        for field in runtime_identity.PATH_FIELDS:
            bad = baseline(**{field: 'relative/lib.so'})
            with self.subTest(field=field), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(bad, GATEWAY)
            self.assertEqual(str(caught.exception), 'baseline_path_must_be_absolute:' + field)
        with self.assertRaises(ValueError) as caught:
            runtime_identity.validate_baseline(baseline(plugin_version=7), GATEWAY)
        self.assertEqual(str(caught.exception), 'baseline_version_invalid')

    def test_management_boundary_must_share_exact_cpa_origin(self):
        # 同一 CPA 但异端口：在回环检查通过后命中同源校验。
        boundary = 'http://127.0.0.1:9999/v0/management/plugins/commandcode-pool'
        with self.assertRaises(ValueError) as caught:
            runtime_identity.validate_baseline(baseline(management_boundary=boundary), GATEWAY)
        self.assertEqual(str(caught.exception),
                         'probe_requires_same_direct_cpa_management_origin')
        # 非回环管理端点：更早被回环门拒绝（同样零 POST）。
        for boundary in ('http://10.0.0.5:15721/v0/management/plugins/commandcode-pool',
                         'https://127.0.0.1:15721/v0/management/plugins/commandcode-pool'):
            with self.subTest(boundary=boundary), self.assertRaises(ValueError):
                runtime_identity.validate_baseline(baseline(management_boundary=boundary), GATEWAY)
        for boundary in (
                'http://127.0.0.1:15721/v0/management/plugins/commandcode-pool?x=1',
                'http://127.0.0.1:15721/v0/management/plugins/commandcode-pool#frag',
                'http://127.0.0.1:15721/v0/management/elsewhere'):
            with self.subTest(boundary=boundary), self.assertRaises(ValueError) as caught:
                runtime_identity.validate_baseline(baseline(management_boundary=boundary), GATEWAY)
            self.assertEqual(str(caught.exception), 'management_boundary_invalid')

    def test_non_loopback_gateway_is_refused(self):
        with self.assertRaises(ValueError):
            runtime_identity.validate_baseline(baseline(), 'http://api.example.com')


class EpochTests(unittest.TestCase):
    def test_epoch_ignores_parens_and_spaces_in_process_name(self):
        text = stat_text(comm='my (weird) name with (parens)')
        self.assertEqual(runtime_identity.process_epoch(PID, text), '%d:%d' % (PID, STARTTICKS))

    def test_epoch_reflects_live_startticks(self):
        self.assertEqual(runtime_identity.process_epoch(77, stat_text(pid=77, startticks=4242)),
                         '77:4242')


class ProcessIdentityTests(unittest.TestCase):
    def test_reads_live_proc_and_matches_expected_shapes(self):
        with fake_proc(Proc()):
            identity = runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(identity, {'pid': PID, 'startticks_epoch': '%d:%d' % (PID, STARTTICKS),
                                    'core_sha256': 'a' * 64, 'lib_sha256': 'b' * 64,
                                    'config_sha256': 'c' * 64})

    def test_reflects_live_reads_rather_than_static_snapshot(self):
        proc = Proc(stat=stat_text(startticks=555))
        proc.digest[LIBRARY] = 'e' * 64
        with fake_proc(proc):
            identity = runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(identity['startticks_epoch'], '%d:555' % PID)
        self.assertEqual(identity['lib_sha256'], 'e' * 64)

    def test_same_path_replaced_library_is_rejected_by_maps_identity(self):
        # 同一路径但磁盘库已被替换（maps 里的 disk inode/dev 与当前 stat 不符）。
        for proc in (Proc(ino=99999), Proc(dev=os.makedev(8, 2))):
            with self.subTest(proc=vars(proc)), fake_proc(proc), \
                    self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
            self.assertEqual(str(caught.exception), 'mapped_library_file_identity_changed')

    def test_library_not_mapped_is_rejected(self):
        proc = Proc(maps=maps_line(path='/opt/cpa/lib/other.so'))
        with fake_proc(proc):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(str(caught.exception), 'expected_library_not_mapped_or_deleted')

    def test_executable_path_change_is_rejected(self):
        proc = Proc(exe='/opt/cpa/some-other-binary')
        with fake_proc(proc):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(str(caught.exception), 'process_executable_path_changed')

    def test_missing_config_argument_is_rejected(self):
        proc = Proc(cmdline=('%s\x00--listen\x00:15721\x00' % CORE).encode())
        with fake_proc(proc):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(str(caught.exception), 'process_config_argument_missing')

    def test_config_path_change_is_rejected(self):
        proc = Proc(cmdline=('%s\x00--config=/etc/other.yaml\x00' % CORE).encode())
        with fake_proc(proc):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(str(caught.exception), 'process_config_path_changed')

    def test_listener_not_owned_is_rejected(self):
        with fake_proc(Proc(owned=False)):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.process_identity(baseline(), 15721)
        self.assertEqual(str(caught.exception), 'gateway_listener_not_owned_by_expected_cpa')


class CaptureIdentityTests(unittest.TestCase):
    def test_matches_baseline_and_uses_real_process_reads_twice(self):
        with fake_proc(Proc()) as _, \
                mock.patch.object(runtime_identity, 'process_identity',
                                  wraps=runtime_identity.process_identity) as observed:
            identity = runtime_identity.capture_identity(baseline(), GATEWAY, 'public-mgmt')
        self.assertEqual(observed.call_count, 2)  # before + after，非一次静态快照
        self.assertEqual(identity['pid'], PID)
        self.assertEqual(identity['plugin_version'], '0.1.8-local')
        self.assertEqual(identity['management_boundary'], BOUNDARY)

    def test_library_digest_mismatch_is_reported(self):
        proc = Proc()
        proc.digest[LIBRARY] = 'f' * 64
        with fake_proc(proc):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.capture_identity(baseline(), GATEWAY, 'public-mgmt')
        self.assertEqual(str(caught.exception), 'identity_mismatch:lib_sha256')

    def test_plugin_version_mismatch_is_reported(self):
        with fake_proc(Proc(), management={'status': {'plugin_version': '9.9.9-other'}}):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.capture_identity(baseline(), GATEWAY, 'public-mgmt')
        self.assertEqual(str(caught.exception), 'identity_mismatch:plugin_version')

    def test_drift_between_before_and_after_is_detected(self):
        first = {'pid': PID, 'startticks_epoch': '%d:%d' % (PID, STARTTICKS),
                 'core_sha256': 'a' * 64, 'lib_sha256': 'b' * 64, 'config_sha256': 'c' * 64}
        second = dict(first, pid=PID + 1)
        with mock.patch.object(runtime_identity, 'process_identity', side_effect=[first, second]), \
                mock.patch.object(runtime_identity, 'management_get',
                                  return_value={'status': {'plugin_version': '0.1.8-local'}}):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.capture_identity(baseline(), GATEWAY, 'public-mgmt')
        self.assertEqual(str(caught.exception), 'identity_changed_during_preflight')


class DigestTests(unittest.TestCase):
    def test_digest_matches_sha256_without_exposing_content(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'lib.bin'
            payload = b'public-synthetic-library-bytes'
            path.write_bytes(payload)
            self.assertEqual(runtime_identity._digest(path),
                             hashlib.sha256(payload).hexdigest())


class ManagementGetTests(unittest.TestCase):
    def _connection(self, status, body, content_length=None):
        response = mock.Mock()
        response.status = status
        response.read.return_value = body
        connection = mock.Mock()
        connection.getresponse.return_value = response
        return connection

    def test_reads_object_and_sends_bearer_without_leaking(self):
        body = json.dumps({'status': {'plugin_version': '0.1.8-local'}}).encode()
        connection = self._connection(200, body)
        with mock.patch.object(runtime_identity.http.client, 'HTTPConnection',
                               return_value=connection):
            value = runtime_identity.management_get('127.0.0.1', 15721, '/x', 'public-secret-mgmt')
        self.assertEqual(value['status']['plugin_version'], '0.1.8-local')
        self.assertNotIn('public-secret-mgmt', json.dumps(value))
        assert_no_secret(self, value)
        connection.close.assert_called_once()

    def test_non_200_and_non_object_are_rejected(self):
        with mock.patch.object(runtime_identity.http.client, 'HTTPConnection',
                               return_value=self._connection(500, b'{}')):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.management_get('127.0.0.1', 15721, '/x', 'k')
        self.assertEqual(str(caught.exception), 'management_read_failed')
        with mock.patch.object(runtime_identity.http.client, 'HTTPConnection',
                               return_value=self._connection(200, b'[1,2,3]')):
            with self.assertRaises(ValueError) as caught:
                runtime_identity.management_get('127.0.0.1', 15721, '/x', 'k')
        self.assertEqual(str(caught.exception), 'management_response_not_object')


if __name__ == '__main__':
    unittest.main(verbosity=2)
