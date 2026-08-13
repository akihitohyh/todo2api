#!/usr/bin/env bash

set -Eeuo pipefail

readonly ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly WEB_DIR="${ROOT_DIR}/web"

arch="${TARGET_ARCH:-}"
output="${OUTPUT:-}"
skip_web="${SKIP_WEB_BUILD:-0}"
skip_tests="${SKIP_TESTS:-0}"
temp_binary=""

usage() {
  cat <<'EOF'
Build the todo2api Linux binary with the embedded WebUI.

Usage:
  ./build.sh [options]

Options:
  -a, --arch ARCH     Target Go architecture (default: host GOARCH)
  -o, --output FILE   Output file (default: build/todo2api-linux-ARCH)
      --skip-web      Reuse the existing web/dist directory
      --skip-tests    Do not run go test ./...
  -h, --help          Show this help

Environment variables:
  TARGET_ARCH, OUTPUT, SKIP_WEB_BUILD=0|1, SKIP_TESTS=0|1
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

cleanup() {
  if [[ -n "${temp_binary}" && -f "${temp_binary}" ]]; then
    rm -f -- "${temp_binary}"
  fi
}

while (($# > 0)); do
  case "$1" in
    -a | --arch)
      (($# >= 2)) || die "$1 requires a value"
      arch="$2"
      shift 2
      ;;
    --arch=*)
      arch="${1#*=}"
      shift
      ;;
    -o | --output)
      (($# >= 2)) || die "$1 requires a value"
      output="$2"
      shift 2
      ;;
    --output=*)
      output="${1#*=}"
      shift
      ;;
    --skip-web)
      skip_web=1
      shift
      ;;
    --skip-tests)
      skip_tests=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

[[ "${skip_web}" == "0" || "${skip_web}" == "1" ]] || die "SKIP_WEB_BUILD must be 0 or 1"
[[ "${skip_tests}" == "0" || "${skip_tests}" == "1" ]] || die "SKIP_TESTS must be 0 or 1"

require_command go
if [[ -z "${arch}" ]]; then
  arch="$(go env GOARCH)"
fi
[[ -n "${arch}" ]] || die "target architecture cannot be empty"

if [[ -z "${output}" ]]; then
  output="${ROOT_DIR}/build/todo2api-linux-${arch}"
elif [[ "${output}" != /* ]]; then
  output="${ROOT_DIR}/${output}"
fi

if [[ "${skip_web}" == "0" ]]; then
  require_command npm
  [[ -f "${WEB_DIR}/package-lock.json" ]] || die "web/package-lock.json is missing"

  printf '==> Installing WebUI dependencies\n'
  (
    cd -- "${WEB_DIR}"
    npm ci
  )

  printf '==> Building WebUI\n'
  (
    cd -- "${WEB_DIR}"
    npm run build
  )
fi

[[ -f "${WEB_DIR}/dist/index.html" ]] || die "web/dist is missing; run without --skip-web"

if [[ "${skip_tests}" == "0" ]]; then
  printf '==> Running Go tests\n'
  (
    cd -- "${ROOT_DIR}"
    go test ./...
  )
fi

mkdir -p -- "$(dirname -- "${output}")"
temp_binary="${output}.tmp.$$"
trap cleanup EXIT INT TERM

printf '==> Building Linux/%s binary\n' "${arch}"
(
  cd -- "${ROOT_DIR}"
  CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
    go build -trimpath -ldflags='-s -w' -o "${temp_binary}" ./cmd/todo2api
)
chmod 0755 "${temp_binary}"
mv -f -- "${temp_binary}" "${output}"
temp_binary=""

if command -v sha256sum >/dev/null 2>&1; then
  (
    cd -- "$(dirname -- "${output}")"
    sha256sum "$(basename -- "${output}")" >"$(basename -- "${output}").sha256"
  )
fi

printf '==> Build complete: %s\n' "${output}"
