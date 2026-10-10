"""本地证据、身份摘要、独占输出与私有文件读取；网络传输由显式 live 路径负责。"""
import hashlib
import json
import os
import re
import stat
import uuid
from pathlib import Path

# 标准UUID字符串：插件把宿主typed TraceID透传为这种形状。
UUID_PATTERN = (
    r'[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}'
    r'-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}'
)
# 校验用：整串必须恰好是一个UUID。
UUID_RE = re.compile(r'\A(?:' + UUID_PATTERN + r')\Z')
# 提取用：从更长的头部值里找出其中的UUID，允许前后有其它文本。
UUID_SEARCH_RE = re.compile(UUID_PATTERN)

# 真实UDP/TCP之外的任何地址都不允许：探针只连本机 IPv4 回环字面量。
# 刻意不接受 `localhost`（可被 /etc/hosts 改写）或 IPv6，保持目标唯一可审计。
LOOPBACK_HOSTS = ('127.0.0.1',)

# 慢响应的完整缓存上限：超过即判为“响应不完整”，不把截断当成功。
MAX_RESPONSE_BYTES = 2 * 1024 * 1024


def canonical_sha256(value):
    """对结构化数据做稳定摘要，供身份与配置指纹比对使用。"""
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(',', ':'), ensure_ascii=False).encode()
    ).hexdigest()


def file_sha256(path):
    """计算文件摘要；path 为 None 或文件不存在时返回 None，不抛异常。"""
    if path is None:
        return None
    try:
        data = Path(path).read_bytes()
    except OSError:
        return None
    return hashlib.sha256(data).hexdigest()


def load_json(path, limit=32 * 1024 * 1024):
    """读取一个明确路径的JSON证据文件，拒绝超大文件与非法编码。"""
    p = Path(path)
    size = p.stat().st_size
    if size > limit:
        raise ValueError('evidence_too_large: %s' % p)
    if p.is_symlink():
        raise ValueError('evidence_must_not_be_symlink: %s' % p)
    return json.loads(p.read_text(encoding='utf-8'))


def _sync_parent(path):
    fd = os.open(Path(path).parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def atomic_write_new(path, value):
    """O_EXCL 独占创建：evidence 存在时拒绝覆盖，原文件保持不动。"""
    payload = json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True).encode('utf-8')
    fd = os.open(str(path), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())
    _sync_parent(path)


def atomic_replace_private(path, value):
    """先写同目录临时文件再替换，保证崩溃后 journal 至少是上一次完整状态。"""
    target = Path(path)
    tmp = target.with_name(target.name + '.tmp-' + uuid.uuid4().hex)
    payload = json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True).encode('utf-8')
    fd = os.open(str(tmp), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(payload)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, target)
    _sync_parent(target)


def fingerprint_identity(identity):
    """把身份快照归一成可比对的指纹；只保留摘要，不保留原文敏感字段。

    身份快照允许的字段全部是哈希/数字/布尔，调用方负责不要在这里塞入密钥或账号明文。
    """
    if not isinstance(identity, dict):
        raise ValueError('identity_must_be_object')
    normalized = {}
    for key, value in identity.items():
        if isinstance(value, (str, int, float, bool)) or value is None:
            normalized[key] = value
        elif isinstance(value, (list, dict)):
            normalized[key] = canonical_sha256(value)
        else:
            raise ValueError('identity_field_unusable: %s' % key)
    return normalized


def compare_identity(fields, baseline):
    """逐字段核对身份基线，返回不一致字段名列表；空列表代表完全一致。"""
    if not isinstance(baseline, dict):
        raise ValueError('baseline_missing_or_invalid')
    mismatched = []
    for key, expected in baseline.items():
        actual = fields.get(key)
        if isinstance(actual, (list, dict)) or isinstance(expected, (list, dict)):
            same = actual is not None and canonical_sha256(actual) == canonical_sha256(expected)
        else:
            same = actual == expected
        if not same:
            mismatched.append(key)
    return mismatched


def bounded_int(name, value, minimum, maximum):
    """校验一个受控整数参数必须在闭区间内，否则抛错，绝不静默夹取。"""
    if not isinstance(value, int) or isinstance(value, bool):
        raise ValueError('%s_must_be_integer' % name)
    if value < minimum or value > maximum:
        raise ValueError('%s_out_of_range:%s..%s' % (name, minimum, maximum))
    return value


def parse_timestamp(value):
    """解析带时区的ISO时间；无时区视为无效，避免时区猜测带来的假边界。"""
    import datetime as dt

    if not isinstance(value, str):
        raise ValueError('timestamp_must_be_string')
    parsed = dt.datetime.fromisoformat(value.replace('Z', '+00:00'))
    if parsed.tzinfo is None:
        raise ValueError('timestamp_requires_timezone')
    return parsed


def require_loopback_url(url):
    """只允许回环地址的网关URL，阻止把探针指向真实网络或生产主机。"""
    from urllib.parse import urlsplit

    parts = urlsplit(url)
    if parts.scheme != 'http':
        raise ValueError('gateway_scheme_must_be_http')
    if parts.username is not None or parts.password is not None:
        raise ValueError('gateway_url_must_not_carry_credentials')
    if parts.hostname not in LOOPBACK_HOSTS:
        raise ValueError('gateway_host_must_be_loopback')
    if parts.path not in ('', '/'):
        raise ValueError('gateway_url_must_not_carry_path')
    if parts.query or parts.fragment:
        raise ValueError('gateway_url_must_not_carry_query')
    port = parts.port if parts.port is not None else 80
    bounded_int('gateway_port', port, 1, 65535)
    return parts.hostname, port


def read_private_file(path, max_bytes=4096):
    """同一文件描述符检查并读取当前用户的 0600 常规文件，拒绝链接、FIFO 和控制字符。"""
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode):
            raise ValueError('secret_file_must_be_regular_file')
        if stat.S_IMODE(info.st_mode) != 0o600 or info.st_uid != os.getuid():
            raise ValueError('secret_file_mode_and_owner_must_be_private')
        if not 0 < info.st_size <= max_bytes:
            raise ValueError('secret_file_size_out_of_range')
        raw = stream.read(max_bytes + 1)
        if len(raw) > max_bytes:
            raise ValueError('secret_file_size_out_of_range')
    value = raw.decode('utf-8').strip()
    if not value or any(ord(ch) < 32 or ord(ch) == 127 for ch in value):
        raise ValueError('secret_file_value_invalid')
    return value
