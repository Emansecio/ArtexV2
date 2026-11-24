#!/usr/bin/env bash
# Fetch pinned static Go security binaries (nuclei, httpx, subfinder, katana,
# ffuf, dalfox) for a target OS/ARCH and pack them into toolbox/embedded/toolbox.tar.zst
# so `go build` embeds them directly inside the artex executable.
set -euo pipefail

cd "$(cd "$(dirname "$0")/.." && pwd)"

NUCLEI_VERSION="v3.11.1"
HTTPX_VERSION="v1.12.0"
SUBFINDER_VERSION="v2.17.0"
KATANA_VERSION="v1.8.0"
FFUF_VERSION="v2.3.0"
DALFOX_VERSION="v3.2.4"
DNSX_VERSION="v1.3.1"
TLSX_VERSION="v1.5.0"
NAABU_VERSION="v2.7.0"
GAU_VERSION="v2.2.4"
GOJQ_VERSION="v0.12.20"

# Pinned SHA-256 digests for Linux release assets (migrated from Dockerfile).
NUCLEI_SHA256_LINUX_AMD64="ea63d4ae232808cd7c6bc00d0142428e231fab59dae01042246097d195835ab6"
NUCLEI_SHA256_LINUX_ARM64="8044e3d9768ba0a744b2872c1a87e813006f013da97ca9f50f7661a4203bec07"
HTTPX_SHA256_LINUX_AMD64="9d8439e8b6c9aa7d1e2314817a392e00d5178da3af5652f7475f88868f418f76"
HTTPX_SHA256_LINUX_ARM64="fd7b123c1dfbc3d69f19f524e4eebcd6ec06b9a6cbd56813c76f11645197331e"
SUBFINDER_SHA256_LINUX_AMD64="a39c3e5c7a26ef3c430915c4d6cbf7716a00f26769a75e9405aba796142eba37"
SUBFINDER_SHA256_LINUX_ARM64="24913d49dddc1d3a5e99a4a7580da5be980959b9415b96f52c4f7f4124392190"
KATANA_SHA256_LINUX_AMD64="cb027ae99d0757061b70c07a15c7a80f62c5a535ab52c4a329f80c4b0a7309d2"
KATANA_SHA256_LINUX_ARM64="80e269210727a855511ffac8dfb01297b2344742acec71a35e8ba4a67a90f0f2"
FFUF_SHA256_LINUX_AMD64="b2a3c725fcb9da175159682f54d6e9149f2905b00d84d155e7efc5d599975ceb"
FFUF_SHA256_LINUX_ARM64="e2d712615d53da797ee929006e41f2df5a7960a5353133db9c150ddaa22ae4e8"
DALFOX_SHA256_LINUX_AMD64="c11cfd2f8261973f0a2a2a1eba5a147ce77712a29acfa6343f9847fb8bb7eca2"
DALFOX_SHA256_LINUX_ARM64="fd56b0b7da2d06868cd73b504f41d21314629feffcd5bdc97718db1c9dd2fa70"

TARGET=""
OUT="toolbox/embedded/toolbox.tar.zst"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --target)
      TARGET="$2"
      shift 2
      ;;
    --out)
      OUT="$2"
      shift 2
      ;;
    *)
      echo "usage: $0 [--target OS/ARCH] [--out path]" >&2
      exit 2
      ;;
  esac
done

if [ -z "$TARGET" ]; then
  TARGET="$(go env GOOS)/$(go env GOARCH)"
fi

GOOS="${TARGET%%/*}"
GOARCH="${TARGET##*/}"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

BIN_EXT=""
case "${GOOS}/${GOARCH}" in
  linux/amd64)
    PD_OS="linux"; PD_ARCH="amd64"
    FFUF_OS="linux"; FFUF_ARCH="amd64"
    DALFOX_ASSET="dalfox-${DALFOX_VERSION}-linux-x86_64-musl.tar.gz"
    NAABU_ASSET="naabu_${NAABU_VERSION#v}_linux_amd64_musl.zip"
    GAU_ASSET="gau_${GAU_VERSION#v}_linux_amd64.tar.gz"
    GOJQ_ASSET="gojq_${GOJQ_VERSION}_linux_amd64.tar.gz"
    NUCLEI_SUM="$NUCLEI_SHA256_LINUX_AMD64"
    HTTPX_SUM="$HTTPX_SHA256_LINUX_AMD64"
    SUBFINDER_SUM="$SUBFINDER_SHA256_LINUX_AMD64"
    KATANA_SUM="$KATANA_SHA256_LINUX_AMD64"
    FFUF_SUM="$FFUF_SHA256_LINUX_AMD64"
    DALFOX_SUM="$DALFOX_SHA256_LINUX_AMD64"
    ;;
  linux/arm64)
    PD_OS="linux"; PD_ARCH="arm64"
    FFUF_OS="linux"; FFUF_ARCH="arm64"
    DALFOX_ASSET="dalfox-${DALFOX_VERSION}-linux-aarch64-musl.tar.gz"
    NAABU_ASSET="naabu_${NAABU_VERSION#v}_linux_arm64_musl.zip"
    GAU_ASSET="gau_${GAU_VERSION#v}_linux_arm64.tar.gz"
    GOJQ_ASSET="gojq_${GOJQ_VERSION}_linux_arm64.tar.gz"
    NUCLEI_SUM="$NUCLEI_SHA256_LINUX_ARM64"
    HTTPX_SUM="$HTTPX_SHA256_LINUX_ARM64"
    SUBFINDER_SUM="$SUBFINDER_SHA256_LINUX_ARM64"
    KATANA_SUM="$KATANA_SHA256_LINUX_ARM64"
    FFUF_SUM="$FFUF_SHA256_LINUX_ARM64"
    DALFOX_SUM="$DALFOX_SHA256_LINUX_ARM64"
    ;;
  darwin/arm64)
    PD_OS="macOS"; PD_ARCH="arm64"
    FFUF_OS="macOS"; FFUF_ARCH="arm64"
    DALFOX_ASSET="dalfox-${DALFOX_VERSION}-macos-aarch64.tar.gz"
    NAABU_ASSET="naabu_${NAABU_VERSION#v}_macOS_arm64.zip"
    GAU_ASSET="gau_${GAU_VERSION#v}_darwin_arm64.tar.gz"
    GOJQ_ASSET="gojq_${GOJQ_VERSION}_darwin_arm64.zip"
    NUCLEI_SUM=""; HTTPX_SUM=""; SUBFINDER_SUM=""; KATANA_SUM=""; FFUF_SUM=""; DALFOX_SUM=""
    ;;
  darwin/amd64)
    PD_OS="macOS"; PD_ARCH="amd64"
    FFUF_OS="macOS"; FFUF_ARCH="amd64"
    DALFOX_ASSET="dalfox-${DALFOX_VERSION}-macos-x86_64.tar.gz"
    NAABU_ASSET="naabu_${NAABU_VERSION#v}_macOS_amd64.zip"
    GAU_ASSET="gau_${GAU_VERSION#v}_darwin_amd64.tar.gz"
    GOJQ_ASSET="gojq_${GOJQ_VERSION}_darwin_amd64.zip"
    NUCLEI_SUM=""; HTTPX_SUM=""; SUBFINDER_SUM=""; KATANA_SUM=""; FFUF_SUM=""; DALFOX_SUM=""
    ;;
  windows/amd64)
    PD_OS="windows"; PD_ARCH="amd64"
    FFUF_OS="windows"; FFUF_ARCH="amd64"
    DALFOX_ASSET="dalfox-${DALFOX_VERSION}-windows-x86_64.zip"
    NAABU_ASSET="naabu_${NAABU_VERSION#v}_windows_amd64.zip"
    GAU_ASSET="gau_${GAU_VERSION#v}_windows_amd64.zip"
    GOJQ_ASSET="gojq_${GOJQ_VERSION}_windows_amd64.zip"
    BIN_EXT=".exe"
    NUCLEI_SUM=""; HTTPX_SUM=""; SUBFINDER_SUM=""; KATANA_SUM=""; FFUF_SUM=""; DALFOX_SUM=""
    ;;
  *)
    echo "unsupported toolbox target: ${GOOS}/${GOARCH}" >&2
    exit 1
    ;;
esac

WORK="$(mktemp -d "${TMPDIR:-/tmp}/artex-toolbox-fetch.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
STAGE="${WORK}/bin"
mkdir -p "$STAGE"

fetch_tool() {
  base="$1"
  asset="$2"
  want="$3"
  name="${4}${BIN_EXT}"
  dl="${WORK}/${asset}"
  ext="${WORK}/extract-${4}"
  mkdir -p "$ext"

  echo "[toolbox] fetching ${asset} (${GOOS}/${GOARCH})"
  curl -fsSL "${base}/${asset}" -o "$dl"
  got="$(sha256_file "$dl")"
  if [ -n "$want" ] && [ "$want" != "$got" ]; then
    echo "checksum mismatch for ${asset}: want ${want} got ${got}" >&2
    exit 1
  fi
  case "$asset" in
    *.zip) unzip -q "$dl" -d "$ext" ;;
    *) tar -xzf "$dl" -C "$ext" ;;
  esac
  found="$(find "$ext" -type f -name "$name" | head -n 1)"
  if [ -z "$found" ]; then
    echo "${name} not found in ${asset}" >&2
    exit 1
  fi
  install -m 0755 "$found" "${STAGE}/${name}"
}

GH="https://github.com"
fetch_tool "${GH}/projectdiscovery/nuclei/releases/download/${NUCLEI_VERSION}" \
  "nuclei_${NUCLEI_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "$NUCLEI_SUM" nuclei
fetch_tool "${GH}/projectdiscovery/httpx/releases/download/${HTTPX_VERSION}" \
  "httpx_${HTTPX_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "$HTTPX_SUM" httpx
fetch_tool "${GH}/projectdiscovery/subfinder/releases/download/${SUBFINDER_VERSION}" \
  "subfinder_${SUBFINDER_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "$SUBFINDER_SUM" subfinder
fetch_tool "${GH}/projectdiscovery/katana/releases/download/${KATANA_VERSION}" \
  "katana_${KATANA_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "$KATANA_SUM" katana
fetch_tool "${GH}/ffuf/ffuf/releases/download/${FFUF_VERSION}" \
  "ffuf_${FFUF_VERSION#v}_${FFUF_OS}_${FFUF_ARCH}.tar.gz" "$FFUF_SUM" ffuf
fetch_tool "${GH}/hahwul/dalfox/releases/download/${DALFOX_VERSION}" \
  "$DALFOX_ASSET" "$DALFOX_SUM" dalfox
fetch_tool "${GH}/projectdiscovery/dnsx/releases/download/${DNSX_VERSION}" \
  "dnsx_${DNSX_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "" dnsx
fetch_tool "${GH}/projectdiscovery/tlsx/releases/download/${TLSX_VERSION}" \
  "tlsx_${TLSX_VERSION#v}_${PD_OS}_${PD_ARCH}.zip" "" tlsx
fetch_tool "${GH}/projectdiscovery/naabu/releases/download/${NAABU_VERSION}" \
  "$NAABU_ASSET" "" naabu
fetch_tool "${GH}/lc/gau/releases/download/${GAU_VERSION}" \
  "$GAU_ASSET" "" gau
fetch_tool "${GH}/itchyny/gojq/releases/download/${GOJQ_VERSION}" \
  "$GOJQ_ASSET" "" gojq

echo "[toolbox] packing -> ${OUT}"
go run ./scripts/pack_toolbox.go "$STAGE" "$OUT"
echo "[toolbox] ready: ${OUT} ($(wc -c < "$OUT" | tr -d ' ') bytes)"
