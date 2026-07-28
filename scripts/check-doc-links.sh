#!/bin/sh
set -eu

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
paths_file=$(mktemp "${TMPDIR:-/tmp}/wrap-doc-paths.XXXXXX")
links_file=$(mktemp "${TMPDIR:-/tmp}/wrap-doc-links.XXXXXX")

cleanup() {
  rm -f "$paths_file" "$links_file"
}
trap cleanup 0 1 2 15

status=0

find "$repo" -name '*.md' \
  -not -path "$repo/.agents/*" \
  -not -path "$repo/.superpowers/*" \
  -not -path "$repo/.flox/*" > "$paths_file"

while IFS= read -r file; do
  base=$(dirname "$file")
  if grep -Eo '\[[^]]+\]\([^)]+\)' "$file" > "$links_file"; then
    :
  else
    grep_status=$?
    if [ "$grep_status" -eq 1 ]; then
      : > "$links_file"
    else
      echo "$file: grep failed with status $grep_status" >&2
      status=1
      continue
    fi
  fi
  while IFS= read -r match; do
    link=${match#*(}
    link=${link%)}
    case "$link" in
      http://*|https://*|mailto:*|\#*|'') continue ;;
    esac
    target=${link%%#*}
    if [ ! -e "$base/$target" ]; then
      echo "$file: broken relative link: $link" >&2
      status=1
    fi
  done < "$links_file"
done < "$paths_file"

exit "$status"
