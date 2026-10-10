"""在每次探针前核对真实进程、已加载库、配置和管理端口，不信静态快照。"""
import hashlib
import http.client
import json
import os
import re
from pathlib import Path
from urllib.parse import urlsplit

from evidence import require_loopback_url

IDENTITY_FIELDS = ('pid', 'startticks_epoch', 'core_sha256', 'lib_sha256',
                   'config_sha256', 'plugin_version', 'management_boundary')
PATH_FIELDS = ('core_path', 'library_path', 'config_path')
SHA256 = re.compile(r'^[0-9a-f]{64}$')
PLUGIN_PATH = re.compile(r'^/v0/management/plugins/commandcode-pool(?:-next|-update)?$')


def validate_baseline(baseline, gateway):
    """只允许已确认的直接 CPA 回环端点与完整可验证的身份基线。"""
    if not isinstance(baseline, dict) or baseline.get('identity_confirmed') is not True:
        raise ValueError('identity_not_confirmed')
    pid = baseline.get('pid')
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        raise ValueError('baseline_pid_invalid')
    for field in IDENTITY_FIELDS + PATH_FIELDS:
        if not baseline.get(field):
            raise ValueError('baseline_field_missing:' + field)
    for field in ('core_sha256', 'lib_sha256', 'config_sha256'):
        if not isinstance(baseline[field], str) or not SHA256.fullmatch(baseline[field]):
            raise ValueError('baseline_digest_invalid:' + field)
    if not re.fullmatch(str(pid) + r':[0-9]+', str(baseline['startticks_epoch'])):
        raise ValueError('baseline_epoch_invalid')
    for field in PATH_FIELDS:
        if not isinstance(baseline[field], str) or not Path(baseline[field]).is_absolute():
            raise ValueError('baseline_path_must_be_absolute:' + field)
    if not isinstance(baseline['plugin_version'], str):
        raise ValueError('baseline_version_invalid')
    host, port = require_loopback_url(gateway)
    management = urlsplit(baseline['management_boundary'])
    origin = '%s://%s' % (management.scheme, management.netloc)
    mhost, mport = require_loopback_url(origin)
    if (mhost, mport) != (host, port):
        raise ValueError('probe_requires_same_direct_cpa_management_origin')
    if management.query or management.fragment or not PLUGIN_PATH.fullmatch(management.path):
        raise ValueError('management_boundary_invalid')
    return host, port, management.path


def _digest(path):
    """只计算摘要，不输出文件内容，配置与库均只在受控内存读取。"""
    digest = hashlib.sha256()
    with open(path, 'rb') as stream:
        while chunk := stream.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def process_epoch(pid, stat_text):
    """从完整 stat 中解析 startticks，进程名含空格或括号也不混淆字段。"""
    fields = stat_text.rsplit(')', 1)[1].split()
    return '%s:%s' % (pid, fields[19])


def owns_listener(pid, port):
    """核对回环目标监听 socket 确实由指定 CPA 进程持有。"""
    proc = Path('/proc') / str(pid)
    socket_ids = set()
    for entry in (proc / 'fd').iterdir():
        try:
            target = os.readlink(entry)
        except FileNotFoundError:
            continue
        match = re.fullmatch(r'socket:\[([0-9]+)\]', target)
        if match:
            socket_ids.add(match.group(1))
    for line in (proc / 'net/tcp').read_text().splitlines()[1:]:
        fields = line.split()
        address, hex_port = fields[1].split(':')
        if (fields[3] == '0A' and int(hex_port, 16) == port
                and address in ('0100007F', '00000000') and fields[9] in socket_ids):
            return True
    return False


def process_identity(baseline, port):
    """读取活动进程并确认其执行文件、配置参数和内存映射库，不代用磁盘副本。"""
    pid = baseline['pid']
    proc = Path('/proc') / str(pid)
    core = Path(baseline['core_path']).resolve(strict=True)
    library = Path(baseline['library_path']).resolve(strict=True)
    config = Path(baseline['config_path']).resolve(strict=True)
    if Path(os.readlink(proc / 'exe')) != core:
        raise ValueError('process_executable_path_changed')
    args = (proc / 'cmdline').read_bytes().decode().split('\0')
    configured = None
    for i, argument in enumerate(args):
        if argument == '--config' and i + 1 < len(args):
            configured = args[i + 1]
        elif argument.startswith('--config='):
            configured = argument.split('=', 1)[1]
    if not configured:
        raise ValueError('process_config_argument_missing')
    actual_config = Path(configured)
    if not actual_config.is_absolute():
        actual_config = Path(os.readlink(proc / 'cwd')) / actual_config
    if actual_config.resolve(strict=True) != config:
        raise ValueError('process_config_path_changed')
    mapped = False
    library_stat = library.stat()
    for line in (proc / 'maps').read_text().splitlines():
        fields = line.split(None, 5)
        if len(fields) != 6:
            continue
        path = re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), fields[5])
        if path == str(library):
            major, minor = (int(value, 16) for value in fields[3].split(':'))
            if (int(fields[4]) != library_stat.st_ino
                    or (major, minor) != (os.major(library_stat.st_dev), os.minor(library_stat.st_dev))):
                raise ValueError('mapped_library_file_identity_changed')
            mapped = True
            break
    if not mapped:
        raise ValueError('expected_library_not_mapped_or_deleted')
    if not owns_listener(pid, port):
        raise ValueError('gateway_listener_not_owned_by_expected_cpa')
    return {'pid': pid, 'startticks_epoch': process_epoch(pid, (proc / 'stat').read_text()),
            'core_sha256': _digest(proc / 'exe'), 'lib_sha256': _digest(library),
            'config_sha256': _digest(config)}


def management_get(host, port, path, key):
    """只读同一 CPA 的管理 JSON，拒绝重定向及超大正文，不保存鉴权值。"""
    connection = http.client.HTTPConnection(host, port, timeout=10)
    try:
        connection.request('GET', path, headers={'Authorization': 'Bearer ' + key})
        response = connection.getresponse()
        raw = response.read(2 * 1024 * 1024 + 1)
        if response.status != 200 or len(raw) > 2 * 1024 * 1024:
            raise ValueError('management_read_failed')
        value = json.loads(raw)
        if not isinstance(value, dict):
            raise ValueError('management_response_not_object')
        return value
    finally:
        connection.close()


def capture_identity(baseline, gateway, management_key):
    """在请求前后都读真实进程身份，阻断重启或管理调用期间的漂移。"""
    host, port, prefix = validate_baseline(baseline, gateway)
    before = process_identity(baseline, port)
    status = management_get(host, port, prefix + '/status', management_key).get('status', {})
    after = process_identity(baseline, port)
    if before != after:
        raise ValueError('identity_changed_during_preflight')
    after.update({'plugin_version': status.get('plugin_version'),
                  'management_boundary': baseline['management_boundary']})
    for field in IDENTITY_FIELDS:
        if after.get(field) != baseline[field]:
            raise ValueError('identity_mismatch:' + field)
    return after
