#!/bin/sh
# Wrapper that turns `zig cc` into a cross-compiling CC for cgo builds.
#
# go's cgo build sets GOOS/GOARCH in the environment of the compiler
# invocation, so this script reads those to pick zig's target triple and
# forwards all other arguments to `zig cc` unchanged.
set -e

# Apple system libraries and frameworks come from the Xcode SDK, which Zig
# does not bundle. On a Darwin release host, use the SDK-aware native compiler
# for both Darwin architectures; Go supplies the appropriate -arch flag.
if [ "${GOOS}" = "darwin" ] && [ "$(uname -s)" = "Darwin" ]; then
  exec xcrun --sdk macosx clang "$@"
fi

script_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
# shellcheck source=zig-common.sh
. "$script_dir/zig-common.sh"

target=$(mamari_zig_target)
mamari_prepare_zig_cache

exec zig cc -target "${target}" "$@"
