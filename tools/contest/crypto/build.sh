#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname "$0")/../../.." && pwd)
[ "$1/$2" = linux/amd64 ] || { echo 'Native crypto requires linux/amd64' >&2; exit 1; }
docker build --platform linux/amd64 -q -t isucon15-native-crypto:local "$root/tools/contest/crypto"
trap 'rm -f "$root/webapp/go/libisureal-crypto.so.tmp"' EXIT
docker run --rm --platform linux/amd64 isucon15-native-crypto:local cat /build/libisureal-crypto.so > "$root/webapp/go/libisureal-crypto.so.tmp"
mv "$root/webapp/go/libisureal-crypto.so.tmp" "$root/webapp/go/libisureal-crypto.so"
