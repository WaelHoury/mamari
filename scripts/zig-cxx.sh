#!/bin/sh
# C++ counterpart to zig-cc.sh, for grammars whose scanner is written in C++.
set -e

# See zig-cc.sh: Darwin targets need the Apple SDK for system libraries and
# frameworks, while Zig remains the cross-compiler for Linux and Windows.
if [ "${GOOS}" = "darwin" ] && [ "$(uname -s)" = "Darwin" ]; then
  exec xcrun --sdk macosx clang++ "$@"
fi

script_dir=$(CDPATH= cd -P "$(dirname "$0")" && pwd)
# shellcheck source=zig-common.sh
. "$script_dir/zig-common.sh"

target=$(mamari_zig_target)
mamari_prepare_zig_cache

exec zig c++ -target "${target}" "$@"
