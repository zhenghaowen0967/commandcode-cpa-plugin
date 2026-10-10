"""默认离线入口：自包含单测中阻断本进程连接与命中生产路径标记的读取。

审计钩子不继承到子进程，也不是操作系统沙箱；CLI 测试只处理显式合成夹具。
"""
import io
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
TESTS = HERE / 'tests'
sys.path.insert(0, str(HERE))

# 生产路径标记：命中即视为越界读取。全部是“不能碰”的对象，不是可复用输入。
PRODUCTION_MARKERS = (
    'EasyCLIProxyAPI', 'cpa-core', 'cpa-management-key', 'cc-switch.db',
    'usage.sqlite', '/proc/', 'settings.json', 'commandcode-pool-next',
)

VIOLATIONS = []


def _audit(event, args):
    if event == 'socket.connect':
        VIOLATIONS.append(('socket.connect', 'blocked'))
        raise PermissionError('offline_network_blocked')
    elif event == 'open':
        path = args[0] if args else None
        if isinstance(path, str) and any(marker in path for marker in PRODUCTION_MARKERS):
            VIOLATIONS.append(('open', 'blocked'))
            raise PermissionError('offline_production_path_blocked')


def main():
    sys.addaudithook(_audit)
    loader = unittest.TestLoader()
    suite = loader.discover(str(TESTS), pattern='test_*.py', top_level_dir=str(TESTS))
    stream = io.StringIO()
    result = unittest.TextTestRunner(stream=stream, verbosity=2).run(suite)
    print(stream.getvalue())
    print('offline tests: run=%d failures=%d errors=%d skipped=%d' % (
        result.testsRun, len(result.failures), len(result.errors), len(result.skipped)))
    if VIOLATIONS:
        print('offline violations: %s' % VIOLATIONS)
    if result.wasSuccessful() and not VIOLATIONS:
        print('offline: OK (no production reads, no network connections)')
        return 0
    print('offline: FAILED')
    return 1


if __name__ == '__main__':
    sys.exit(main())
