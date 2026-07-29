#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/wrap-doc-links-test.XXXXXX")

cleanup() {
  rm -rf -- "$work"
}
trap cleanup 0 1 2 15

printf '%s\n' '#!/bin/sh' 'exit 2' > "$work/grep"
chmod 0755 "$work/grep"

if PATH="$work:$PATH" "$repo/scripts/check-doc-links.sh" >/dev/null 2>&1; then
  echo "link checker ignored a grep processing failure" >&2
  exit 1
fi
