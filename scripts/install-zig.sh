#!/bin/sh

# Install the exact Zig toolchain used for Mamari release builds from the
# official ziglang.org archive. The archive checksum is deliberately pinned so
# a release does not depend on whichever version Homebrew happens to serve.

set -eu

version=${ZIG_VERSION:-0.16.0}
install_root=${1:-"${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mamari-zig"}

if [ "$version" != "0.16.0" ]; then
  echo "install-zig.sh: unsupported pinned Zig version: $version" >&2
  exit 1
fi

case "$(uname -s)-$(uname -m)" in
  Darwin-x86_64)
    platform=x86_64-macos
    checksum=0387557ed1877bc6a2e1802c8391953baddba76081876301c522f52977b52ba7
    ;;
  Darwin-arm64 | Darwin-aarch64)
    platform=aarch64-macos
    checksum=b23d70deaa879b5c2d486ed3316f7eaa53e84acf6fc9cc747de152450d401489
    ;;
  *)
    echo "install-zig.sh: release builds require macOS on x86_64 or arm64" >&2
    exit 1
    ;;
esac

directory="zig-${platform}-${version}"
destination="${install_root%/}/${directory}"
if [ -x "${destination}/zig" ]; then
  installed_version=$("${destination}/zig" version)
  if [ "$installed_version" = "$version" ]; then
    printf '%s\n' "$destination"
    exit 0
  fi
  echo "install-zig.sh: ${destination}/zig reports ${installed_version}, expected ${version}" >&2
  exit 1
fi

archive="${directory}.tar.xz"
url="https://ziglang.org/download/${version}/${archive}"
mkdir -p "$install_root"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/mamari-zig-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

echo "Downloading Zig ${version} for ${platform}..." >&2
curl --fail --location --retry 3 --silent --show-error \
  "$url" --output "${tmp}/${archive}"

actual=$(shasum -a 256 "${tmp}/${archive}" | awk '{print tolower($1)}')
if [ "$actual" != "$checksum" ]; then
  echo "install-zig.sh: checksum verification failed for ${archive}" >&2
  exit 1
fi

tar -xJf "${tmp}/${archive}" -C "$install_root"
if [ "$("${destination}/zig" version)" != "$version" ]; then
  echo "install-zig.sh: installed Zig failed version verification" >&2
  exit 1
fi

printf '%s\n' "$destination"
