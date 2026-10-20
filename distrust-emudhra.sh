#!/usr/bin/env bash
# Distrust emSign ECC Root CA - G3 in the user's NSS database, as Chromium
# (chromium/chromium@8793082f79) and Ubuntu's ca-certificates 20260816 do,
# after the keys of intermediates under it were compromised
# (https://emudhra.we-have-a-problem.com/). headless-shell reads
# ~/.pki/nssdb but not Chrome's CRLSets.
#
# Distrusting the intermediates instead is not enough: NSS matches exact
# DER, and an intermediate whose ECDSA signature (r, s) is rewritten as
# (r, n-s) still verifies. Chrome matches root store anchors by exact DER
# too, so every path to this key must end at the certificate below.
set -euo pipefail

dir="${HOME}/.pki/nssdb"
install -d -m 700 "${HOME}/.pki" "${dir}"
[[ -f "${dir}/cert9.db" ]] || certutil -N -d "sql:${dir}" --empty-password

# SHA-256 86a1ecba089c4a8d3bbe2734c612ba341d813e043cf9e8a862cd5c57a36bbe6b
certutil -A -d "sql:${dir}" -n "distrusted emSign ECC Root CA - G3" -t p,p,p <<'EOF'
-----BEGIN CERTIFICATE-----
MIICTjCCAdOgAwIBAgIKPPYHqWhwDtqLhDAKBggqhkjOPQQDAzBrMQswCQYDVQQG
EwJJTjETMBEGA1UECxMKZW1TaWduIFBLSTElMCMGA1UEChMcZU11ZGhyYSBUZWNo
bm9sb2dpZXMgTGltaXRlZDEgMB4GA1UEAxMXZW1TaWduIEVDQyBSb290IENBIC0g
RzMwHhcNMTgwMjE4MTgzMDAwWhcNNDMwMjE4MTgzMDAwWjBrMQswCQYDVQQGEwJJ
TjETMBEGA1UECxMKZW1TaWduIFBLSTElMCMGA1UEChMcZU11ZGhyYSBUZWNobm9s
b2dpZXMgTGltaXRlZDEgMB4GA1UEAxMXZW1TaWduIEVDQyBSb290IENBIC0gRzMw
djAQBgcqhkjOPQIBBgUrgQQAIgNiAAQjpQy4LRL1KPOxst3iAhKAnjlfSU2fySU0
WXTsuwYc58Byr+iuL+FBVIcUqEqy6HyC5ltqtdyzdc6LBtCGI79G1Y4PPwT01xyS
fvalY8L1X44uT6EYGQIrMgqCZH0Wk9GjQjBAMB0GA1UdDgQWBBR8XQKEE9TMipuB
zhccLikenEhjQjAOBgNVHQ8BAf8EBAMCAQYwDwYDVR0TAQH/BAUwAwEB/zAKBggq
hkjOPQQDAwNpADBmAjEAvvNhzwIQHWSVB7gYboiFBS+DCBeQyh+KTOgNG3qxrdWB
CUfvO6wIBHxcmbHtRwfSAjEAnbpV/KlK6O3t5nYBQnvI+GDZjVGLVTv7jHvrZQnD
+JbNR6iC8hZVdyR+EhCVBCyj
-----END CERTIFICATE-----
EOF
