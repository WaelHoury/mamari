#!/bin/sh

# Shared setup for the Zig-backed cgo compiler wrappers. Go invokes CC/CXX
# from dependency source directories, which are read-only inside the module
# cache. Zig 0.16 otherwise tries to create a project-local .zig-cache there.

mamari_zig_target() {
  case "${GOOS:-}-${GOARCH:-}" in
    darwin-amd64)  printf '%s\n' x86_64-macos ;;
    darwin-arm64)  printf '%s\n' aarch64-macos ;;
    linux-amd64)   printf '%s\n' x86_64-linux-gnu ;;
    linux-arm64)   printf '%s\n' aarch64-linux-gnu ;;
    windows-amd64) printf '%s\n' x86_64-windows-gnu ;;
    windows-arm64) printf '%s\n' aarch64-windows-gnu ;;
    *)
      echo "mamari Zig wrapper: unsupported GOOS/GOARCH: ${GOOS:-}/${GOARCH:-}" >&2
      return 1
      ;;
  esac
}

mamari_prepare_zig_cache() {
  if [ -z "${ZIG_LOCAL_CACHE_DIR:-}" ]; then
    cache_root=${MAMARI_ZIG_CACHE_ROOT:-"${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mamari-zig-cache"}
    ZIG_LOCAL_CACHE_DIR="${cache_root%/}/${GOOS}-${GOARCH}"
    export ZIG_LOCAL_CACHE_DIR
  fi

  # Create the directory before concurrent cgo compiler processes start.
  # mkdir -p is safe when several invocations race for the same target cache.
  mkdir -p "$ZIG_LOCAL_CACHE_DIR"
}
