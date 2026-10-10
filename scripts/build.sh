#!/usr/bin/env bash
# 只构建本地产物，不安装、发布或启动 CPA。
set -euo pipefail
umask 077
root="$(dirname "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")")"
fail() { printf 'build: %s\n' "$*" >&2; exit 1; }
if (( $# > 1 )); then fail 'usage: build.sh [output-directory]'; fi
if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  printf 'Usage: build.sh [output-directory]\nDefault: <plugin-root>/dist. GO_BIN selects the Go executable.\n'
  exit 0
fi
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || fail 'supported target is native Linux amd64 only'
go_bin="${GO_BIN:-go}"
command -v "$go_bin" >/dev/null || fail 'Go executable not found; set GO_BIN'
command -v "${CC:-gcc}" >/dev/null || fail 'C compiler not found; install gcc or set CC'
command -v sha256sum >/dev/null || fail 'sha256sum not found'
command -v tar >/dev/null || fail 'tar not found'
export GOTOOLCHAIN=local CGO_ENABLED=1 GOOS=linux GOARCH=amd64
version="$("$go_bin" env GOVERSION)"
if [[ $version =~ ^go([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  major=${BASH_REMATCH[1]}; minor=${BASH_REMATCH[2]}; patch=${BASH_REMATCH[3]}
  (( major > 1 || (major == 1 && (minor > 26 || (minor == 26 && patch >= 7))) )) || fail 'Go 1.26.7+ required by go.mod'
else
  fail "unsupported Go version: $version; use a stable Go 1.26.7+ toolchain"
fi
out="$(readlink -m -- "${1:-$root/dist}")"
# 输出目录不得覆盖模块本身、其父目录或源码树。
[[ $out != / && $out != "$root" && $root != "$out/"* ]] || fail 'output directory must not contain the source module'
if [[ $out == "$root/"* && $out != "$root/dist" && $out != "$root/dist/"* ]]; then
  fail 'within the module, output is allowed only under dist/'
fi
repo="$(git -C "$root" rev-parse --show-toplevel 2>/dev/null || true)"
if [[ -n $repo && $out == "$repo/"* && $out != "$root/dist" && $out != "$root/dist/"* && $out != "$repo/.scratch/"* ]]; then
  fail 'within the repository, output is allowed only under plugin dist/ or .scratch/'
fi
plugin_id="${PLUGIN_ID:-commandcode-pool}"
case "$plugin_id" in
  commandcode-pool|commandcode-pool-next|commandcode-pool-update) ;;
  *) fail 'PLUGIN_ID must be commandcode-pool, commandcode-pool-next or commandcode-pool-update' ;;
esac
plugin_role="${PLUGIN_ROLE:-full}"
case "$plugin_role" in
  full|view) ;;
  *) fail 'PLUGIN_ROLE must be full or view' ;;
esac
if [[ $plugin_role == view && $plugin_id != commandcode-pool ]]; then
  fail 'PLUGIN_ROLE=view requires PLUGIN_ID=commandcode-pool'
fi
version_tag=0.1.9-local
artifact_base="$plugin_id"
if [[ $plugin_role == view ]]; then
  version_tag=0.1.1-compatview
  artifact_base="${plugin_id}-v${version_tag}"
fi
archive="${plugin_id}_${version_tag}_linux_amd64.tar.gz"
files=("$artifact_base.so" "$artifact_base.h" LICENSE NOTICE.md THIRD_PARTY_NOTICES.md UNIFIED_SCHEDULER.md README.md VALIDATION.md REGRESSION.md TRACE.md config.example.yaml SHA256SUMS "$archive")
marker="$out/.commandcode-pool-build"
owned=false
[[ ! -L $marker ]] || fail 'refusing a symlink build marker'
if [[ -e $marker ]]; then
  [[ -f $marker && $(<"$marker") == "$root" ]] || fail 'output belongs to a different build/source module'
  owned=true
fi
for name in "${files[@]}"; do
  dest="$out/$name"
  [[ ! -L $dest ]] || fail "refusing symlink artifact: $name"
  [[ ! -e $dest || -f $dest ]] || fail "artifact is not a regular file: $name"
  [[ ! -e $dest || $owned == true ]] || fail "existing artifact is not owned by this script: $name; choose a fresh output directory"
done
for name in LICENSE NOTICE.md THIRD_PARTY_NOTICES.md UNIFIED_SCHEDULER.md README.md VALIDATION.md REGRESSION.md TRACE.md config.example.yaml; do
  [[ -f $root/$name ]] || fail "missing package input: $name"
done
mkdir -p -- "$out"
printf '%s\n' "$root" > "$marker"
# Go -C 固定模块身份，不依赖调用方的当前目录。
"$go_bin" -C "$root" build -buildvcs=false -buildmode=c-shared -trimpath \
  -ldflags "-X commandcode-cpa-plugin/internal/plugin.pluginName=$plugin_id -X commandcode-cpa-plugin/internal/plugin.pluginRole=$plugin_role" \
  -o "$out/$artifact_base.so" .
for name in LICENSE NOTICE.md THIRD_PARTY_NOTICES.md UNIFIED_SCHEDULER.md README.md VALIDATION.md REGRESSION.md TRACE.md config.example.yaml; do
  cp -- "$root/$name" "$out/$name"
done
payload=("$artifact_base.so" "$artifact_base.h" LICENSE NOTICE.md THIRD_PARTY_NOTICES.md UNIFIED_SCHEDULER.md README.md VALIDATION.md REGRESSION.md TRACE.md config.example.yaml)
# 校验清单使用产物目录内的相对路径，便于搬移后核验。
(
  cd -- "$out"
  sha256sum "${payload[@]}" > SHA256SUMS
  tar -czf "$archive" "${payload[@]}" SHA256SUMS
)
printf 'Built %s/%s.so\nPackaged %s/%s\n' "$out" "$artifact_base" "$out" "$archive"
