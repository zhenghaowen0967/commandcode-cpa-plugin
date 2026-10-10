#!/usr/bin/env bash
# 只验证源码和 loopback 夹具，不启动已安装的 CPA/CPAMP。
set -euo pipefail
root="$(dirname "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")")"
fail() { printf 'test: %s\n' "$*" >&2; exit 1; }
if (( $# != 0 )); then fail 'usage: test.sh'; fi
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || fail 'supported target is native Linux amd64 only'
go_bin="${GO_BIN:-go}"
command -v "$go_bin" >/dev/null || fail 'Go executable not found; set GO_BIN'
command -v "${CC:-gcc}" >/dev/null || fail 'C compiler not found; install gcc or set CC'
export GOTOOLCHAIN=local CGO_ENABLED=1 GOOS=linux GOARCH=amd64
version="$("$go_bin" env GOVERSION)"
if [[ $version =~ ^go([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  major=${BASH_REMATCH[1]}; minor=${BASH_REMATCH[2]}; patch=${BASH_REMATCH[3]}
  (( major > 1 || (major == 1 && (minor > 26 || (minor == 26 && patch >= 7))) )) || fail 'Go 1.26.7+ required by go.mod'
else
  fail "unsupported Go version: $version; use a stable Go 1.26.7+ toolchain"
fi
bash "$root/scripts/regression.sh" offline
"$go_bin" -C "$root" test ./...
"$go_bin" -C "$root" test -race ./internal/...
