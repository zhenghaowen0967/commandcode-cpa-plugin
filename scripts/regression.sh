#!/usr/bin/env bash
# 默认离线；live 显式读取指定凭据并访问授权 CPA，不负责安装或启停服务。
set -euo pipefail
root="$(dirname "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")")"
fail() { printf 'regression: %s\n' "$*" >&2; exit 1; }

py_bin="${PYTHON_BIN:-python3}"
command -v "$py_bin" >/dev/null || fail 'python3 not found; set PYTHON_BIN'

command_name="${1:-offline}"
[[ $# -gt 0 ]] && shift

case "$command_name" in
  -h|--help|help)
    cat <<'USAGE'
Usage: bash scripts/regression.sh <command> [options]

Commands:
  offline                     默认：跑自包含单测；无生产读取、无网络连接
  analyze --cases F --observations F --prior-cursor N --out F [more]
                              离线分析既有 JSON 证据；输出已存在则拒绝覆盖
  live [--authorized-live ...] 显式授权的顺序回环探针；默认拒发，不支持全容量
  go                          转发 scripts/test.sh（Python 离线、Go 普通和竞态检查）

环境：
  PYTHON_BIN  指定 Python 3 解释器（默认 python3）
  GO_BIN      仅 go 子命令使用（默认 go）
先运行 `bash scripts/regression.sh offline`；analyze/live 的详细用法见 REGRESSION.md。
USAGE
    exit 0
    ;;
  offline)
    exec "$py_bin" "$root/scripts/regression/offline.py" "$@"
    ;;
  analyze)
    exec "$py_bin" "$root/scripts/regression/analyze.py" "$@"
    ;;
  live)
    exec "$py_bin" "$root/scripts/regression/live.py" "$@"
    ;;
  go)
    exec bash "$root/scripts/test.sh" "$@"
    ;;
  *)
    fail "unknown command: $command_name (see --help)"
    ;;
esac
