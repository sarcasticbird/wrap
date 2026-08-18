#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/wrap-package-test.XXXXXX")

cleanup() {
  rm -rf -- "$work"
}
trap cleanup 0 1 2 15

TZ=Pacific/Honolulu "$repo/scripts/package-release.sh" v0.0.0-test "$work/west"
TZ=Pacific/Kiritimati "$repo/scripts/package-release.sh" v0.0.0-test "$work/east"

diff -u "$work/west/checksums.txt" "$work/east/checksums.txt"

expected_contents='wrap
LICENSE
README.md
SECURITY.md
docs/architecture.md
docs/configuration.md
docs/mobile-mirror-uat.md
docs/troubleshooting.md'

for target in darwin_arm64 linux_amd64 linux_arm64; do
  archive="$work/west/wrap_v0.0.0-test_${target}.tar.gz"
  if [ ! -f "$archive" ]; then
    echo "missing release archive: $archive" >&2
    exit 1
  fi
  contents=$(tar -tzf "$archive")
  if [ "$contents" != "$expected_contents" ]; then
    echo "unexpected contents in $archive:" >&2
    printf '%s\n' "$contents" >&2
    exit 1
  fi
  extract="$work/extract/$target"
  mkdir -p "$extract"
  tar -xzf "$archive" -C "$extract"
  if [ ! -x "$extract/wrap" ]; then
    echo "wrap is not executable in $archive" >&2
    exit 1
  fi
done

if [ -e "$work/west/wrap_v0.0.0-test_darwin_amd64.tar.gz" ]; then
  echo "unsupported Intel macOS archive was produced" >&2
  exit 1
fi

"$repo/scripts/package-release.sh" v0.0.0+build "$work/plus"
if [ ! -f "$work/plus/wrap_v0.0.0+build_darwin_arm64.tar.gz" ]; then
  echo "build-metadata version did not produce the expected archive" >&2
  exit 1
fi

if [ "$(wc -l < "$work/west/checksums.txt" | tr -d '[:space:]')" -ne 3 ]; then
  echo "checksums.txt must contain exactly three archives" >&2
  exit 1
fi
