#!/bin/sh
set -eu

# Exercises scripts/check-release-tag.sh, the gate that keeps a release tag
# from shipping history that never landed on the release branch. The gate is
# business-critical — a tag on an unmerged branch would otherwise publish
# binaries built from unreviewed commits — so every branch of it is covered.

repo=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
gate="$repo/scripts/check-release-tag.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/wrap-tag-test.XXXXXX")

cleanup() {
  rm -rf -- "$work"
}
trap cleanup 0 1 2 15

git -C "$work" init -q -b main
git -C "$work" config user.email test@example.com
git -C "$work" config user.name test

commit() {
  # Deterministic authorship so the temp history never depends on wall clock.
  GIT_AUTHOR_DATE='2026-01-01T00:00:00Z' GIT_COMMITTER_DATE='2026-01-01T00:00:00Z' \
    git -C "$work" commit -q --allow-empty -m "$1"
  git -C "$work" rev-parse HEAD
}

run_gate() {
  # Run the gate from inside the temp repo so its git commands resolve there.
  ( cd "$work" && "$gate" "$@" )
}

main_b=$(commit "A")
main_b=$(commit "B")

# Case 1: annotated tag on the main tip, checked out — the happy path.
git -C "$work" tag -a -m "release 1" v1.0.0
git -C "$work" checkout -q "$main_b"
if ! obj=$(run_gate refs/tags/v1.0.0 main); then
  echo "FAIL (on-main): gate rejected an annotated tag on main" >&2
  exit 1
fi
if [ "$obj" != "$(git -C "$work" rev-parse refs/tags/v1.0.0)" ]; then
  echo "FAIL (on-main): gate printed '$obj', want the tag object" >&2
  exit 1
fi

# Case 2: annotated tag on a branch that never merged to main.
git -C "$work" checkout -q -b side "$main_b~1"
side_c=$(commit "C")
git -C "$work" tag -a -m "release 2" v2.0.0
git -C "$work" checkout -q "$side_c"
if run_gate refs/tags/v2.0.0 main >/dev/null 2>&1; then
  echo "FAIL (off-main): gate accepted a tag not on main" >&2
  exit 1
fi

# Case 3: a lightweight tag is never a release tag.
git -C "$work" checkout -q "$main_b"
git -C "$work" tag v3.0.0
if run_gate refs/tags/v3.0.0 main >/dev/null 2>&1; then
  echo "FAIL (lightweight): gate accepted a non-annotated tag" >&2
  exit 1
fi

# Case 4: an annotated tag that no longer points at HEAD (it moved under us).
git -C "$work" checkout -q "$main_b"
git -C "$work" tag -a -m "release 4" v4.0.0 "$main_b~1"
if run_gate refs/tags/v4.0.0 main >/dev/null 2>&1; then
  echo "FAIL (stale): gate accepted a tag that does not point at HEAD" >&2
  exit 1
fi

echo "check-release-tag: all cases passed"
