#!/bin/sh

set -eu

repository=${MAMARI_REPOSITORY:-waelhoury/mamari}
install_dir=${MAMARI_INSTALL_DIR:-"$HOME/.local/bin"}
version=${MAMARI_VERSION:-}

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *)
    echo "mamari installer: unsupported operating system: $(uname -s)" >&2
    exit 1
    ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *)
    echo "mamari installer: unsupported architecture: $(uname -m)" >&2
    exit 1
    ;;
esac

archive="mamari_${os}_${arch}.tar.gz"
if [ -n "${MAMARI_DOWNLOAD_BASE:-}" ]; then
  base=${MAMARI_DOWNLOAD_BASE%/}
elif [ -n "$version" ]; then
  base="https://github.com/${repository}/releases/download/${version}"
else
  base="https://github.com/${repository}/releases/latest/download"
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mamari-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

download() {
  url=$1
  output=$2
  case "$url" in
    http://* | https://* | file://*) ;;
    *)
      if [ -f "$url" ]; then
        cp "$url" "$output"
        return
      fi
      echo "mamari installer: local release file not found: $url" >&2
      exit 1
      ;;
  esac
  if command -v curl >/dev/null 2>&1; then
    curl --fail --location --silent --show-error "$url" --output "$output"
    return
  fi
  if command -v wget >/dev/null 2>&1; then
    wget --quiet "$url" --output-document="$output"
    return
  fi
  echo "mamari installer: curl or wget is required" >&2
  exit 1
}

echo "Downloading ${archive}..."
download "${base}/${archive}" "${tmp}/${archive}"
download "${base}/checksums.txt" "${tmp}/checksums.txt"

expected=$(
  awk -v filename="$archive" '
    $2 == filename || $2 == "*" filename {
      print tolower($1)
      exit
    }
  ' "${tmp}/checksums.txt"
)
if [ -z "$expected" ]; then
  echo "mamari installer: ${archive} is missing from checksums.txt" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "${tmp}/${archive}" | awk '{print tolower($1)}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "${tmp}/${archive}" | awk '{print tolower($1)}')
else
  echo "mamari installer: sha256sum or shasum is required" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "mamari installer: checksum verification failed for ${archive}" >&2
  exit 1
fi

mkdir -p "${tmp}/unpack"
tar -xzf "${tmp}/${archive}" -C "${tmp}/unpack"
if [ ! -f "${tmp}/unpack/mamari" ]; then
  echo "mamari installer: archive does not contain mamari" >&2
  exit 1
fi

mkdir -p "$install_dir"
if command -v install >/dev/null 2>&1; then
  install -m 0755 "${tmp}/unpack/mamari" "${install_dir}/mamari"
else
  cp "${tmp}/unpack/mamari" "${install_dir}/mamari"
  chmod 0755 "${install_dir}/mamari"
fi

echo "Installed mamari to ${install_dir}/mamari"
case ":${PATH:-}:" in
  *":${install_dir}:"*) ;;
  *)
    echo "Add ${install_dir} to PATH before configuring your MCP client."
    ;;
esac
echo "Next: cd to a codebase and run 'mamari init --mcp <client>'"
