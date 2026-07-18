#!/bin/sh

# Verify a complete GoReleaser output directory before it is trusted by the
# installers or published as a GitHub release.

set -eu

dist=${1:-dist}
required_go=$(awk '$1 == "go" { print "go" $2; exit }' go.mod)
[ -n "$required_go" ] || {
  echo "verify-release.sh: could not read the required Go version from go.mod" >&2
  exit 1
}

fail() {
  echo "verify-release.sh: $*" >&2
  exit 1
}

[ -f "$dist/artifacts.json" ] || fail "missing artifacts.json"
[ -f "$dist/checksums.txt" ] || fail "missing checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$dist" && sha256sum --check checksums.txt)
elif command -v shasum >/dev/null 2>&1; then
  (cd "$dist" && shasum -a 256 --check checksums.txt)
else
  fail "sha256sum or shasum is required"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mamari-release-verify.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

for os in darwin linux windows; do
  for arch in amd64 arm64; do
    if [ "$os" = windows ]; then
      archive="mamari_${os}_${arch}.zip"
      binary=mamari.exe
    else
      archive="mamari_${os}_${arch}.tar.gz"
      binary=mamari
    fi

    [ -f "$dist/$archive" ] || fail "missing $archive"
    unpack="$tmp/${os}_${arch}"
    mkdir -p "$unpack"
    if [ "$os" = windows ]; then
      unzip -q "$dist/$archive" -d "$unpack"
    else
      tar -xzf "$dist/$archive" -C "$unpack"
    fi

    [ -f "$unpack/$binary" ] || fail "$archive does not contain $binary"
    for document in README.md LICENSE CHANGELOG.md SECURITY.md; do
      [ -f "$unpack/$document" ] || fail "$archive does not contain $document"
    done
    [ -f "$unpack/THIRD_PARTY_LICENSES/README.md" ] || \
      fail "$archive does not contain third-party license notices"

    description=$(file "$unpack/$binary")
    case "${os}_${arch}" in
      darwin_amd64)  echo "$description" | grep -q 'Mach-O 64-bit executable x86_64' || fail "$archive has the wrong binary architecture" ;;
      darwin_arm64)  echo "$description" | grep -q 'Mach-O 64-bit executable arm64' || fail "$archive has the wrong binary architecture" ;;
      linux_amd64)   echo "$description" | grep -Eq 'ELF 64-bit.*x86-64' || fail "$archive has the wrong binary architecture" ;;
      linux_arm64)   echo "$description" | grep -Eq 'ELF 64-bit.*(ARM aarch64|aarch64)' || fail "$archive has the wrong binary architecture" ;;
      windows_amd64) echo "$description" | grep -Eq 'PE32\+ executable.*x86-64' || fail "$archive has the wrong binary architecture" ;;
      windows_arm64) echo "$description" | grep -Eq 'PE32\+ executable.*Aarch64' || fail "$archive has the wrong binary architecture" ;;
    esac

    binary_go=$(go version -m "$unpack/$binary" | sed -n '1s/.*: //p')
    [ "$binary_go" = "$required_go" ] || \
      fail "$archive was built with ${binary_go:-an unknown Go version}, expected $required_go"

    if [ "$os" = darwin ]; then
      minos=$(otool -l "$unpack/$binary" | awk '$1 == "minos" { print $2 }' | sort -u)
      [ "$minos" = "12.0" ] || fail "$archive has unexpected macOS minimum version: ${minos:-missing}"
    fi

    if [ "$os" = linux ]; then
      too_new=$(strings "$unpack/$binary" | grep -Eo 'GLIBC_[0-9]+\.[0-9]+' | \
        awk -F'[_.]' '$2 > 2 || ($2 == 2 && $3 > 17) { print; exit }')
      [ -z "$too_new" ] || fail "$archive requires unsupported $too_new (maximum is GLIBC_2.17)"
    fi
  done
done

case "$(uname -s)-$(uname -m)" in
  Darwin-x86_64) native="$tmp/darwin_amd64/mamari" ;;
  Darwin-arm64 | Darwin-aarch64) native="$tmp/darwin_arm64/mamari" ;;
  Linux-x86_64 | Linux-amd64) native="$tmp/linux_amd64/mamari" ;;
  Linux-arm64 | Linux-aarch64) native="$tmp/linux_arm64/mamari" ;;
  *) native= ;;
esac

if [ -n "$native" ]; then
  chmod +x "$native"
  "$native" version
fi

echo "Verified six release archives, checksums, Go version, platform floors, and native execution."
