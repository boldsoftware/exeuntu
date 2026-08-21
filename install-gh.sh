#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
version="${version#v}"
[[ "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "invalid gh version: ${1}" >&2
    exit 1
}

arch="$(dpkg --print-architecture)"
case "${arch}" in
amd64 | arm64) ;;
*)
    echo "unsupported gh architecture: ${arch}" >&2
    exit 1
    ;;
esac

archive="gh_${version}_linux_${arch}.tar.gz"
checksums="gh_${version}_checksums.txt"
release_url="https://github.com/cli/cli/releases/download/v${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

curl -fsSL --retry 5 --retry-delay 2 \
    "${release_url}/${checksums}" -o "${tmp}/${checksums}"
curl -fsSL --retry 5 --retry-delay 2 \
    "${release_url}/${archive}" -o "${tmp}/${archive}"
expected="$(awk -v archive="${archive}" '$2 == archive { print $1 }' "${tmp}/${checksums}")"
[[ "${expected}" =~ ^[0-9a-f]{64}$ ]] || {
    echo "missing checksum for ${archive}" >&2
    exit 1
}
echo "${expected}  ${tmp}/${archive}" | sha256sum -c - >/dev/null

tar -xOzf "${tmp}/${archive}" "gh_${version}_linux_${arch}/bin/gh" >"${tmp}/gh"
install -m 0755 "${tmp}/gh" /usr/local/bin/gh
