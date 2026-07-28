#!/usr/bin/env bash
set -euo pipefail

if [[ ${WRAP_MOCK_GIT:-} == 1 && ${0##*/} == git ]]; then
  printf 'git %s\n' "$*" >>"$WRAP_MOCK_LOG"
  case "${1-}" in
    fetch)
      if [[ -n ${WRAP_MOCK_GIT_FETCH_FAIL_AT:-} ]] &&
        [[ $(grep -Fc "git fetch" "$WRAP_MOCK_LOG") -ge $WRAP_MOCK_GIT_FETCH_FAIL_AT ]]; then
        exit 1
      fi
      ;;
    rev-list)
      tag_sha=$WRAP_MOCK_TAG_SHA
      if [[ -n ${WRAP_MOCK_TAG_MOVE_AT:-} ]] &&
        [[ $(grep -Fc "git rev-list" "$WRAP_MOCK_LOG") -ge $WRAP_MOCK_TAG_MOVE_AT ]]; then
        tag_sha=$WRAP_MOCK_MOVED_TAG_SHA
      fi
      printf '%s\n' "$tag_sha"
      ;;
    rev-parse)
      printf '%s\n' "$WRAP_MOCK_TAG_OBJECT"
      ;;
    for-each-ref)
      printf '%s\n' "Annotated release notes"
      ;;
    *)
      echo "unexpected mock git command: $*" >&2
      exit 2
      ;;
  esac
  exit 0
fi

if [[ ${WRAP_MOCK_GH:-} == 1 ]]; then
  printf '%s\n' "$*" >>"$WRAP_MOCK_LOG"
  case "${1-} ${2-}" in
    "release list")
      printf '%s\n' "${WRAP_MOCK_STATE:-}"
      ;;
    "release create")
      ;;
    "release upload")
      if [[ ${WRAP_MOCK_UPLOAD_FAIL:-} == 1 ]]; then
        exit 1
      fi
      ;;
    "release view")
      if [[ $* == *"--json body"* ]]; then
        printf '%s\n' "${WRAP_MOCK_BODY:-}"
      else
        printf '%s\n' "${WRAP_MOCK_ASSETS:-}"
      fi
      ;;
    "release edit")
      ;;
    *)
      echo "unexpected mock gh command: $*" >&2
      exit 2
      ;;
  esac
  exit 0
fi

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
publisher="$repo/scripts/publish-release.sh"
if ! grep -Eq '^[[:space:]]*cancel-in-progress:[[:space:]]*false([[:space:]]|$)' \
  "$repo/.github/workflows/release.yml"; then
  echo "release workflow must queue same-tag runs instead of cancelling publication checks" >&2
  exit 1
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/wrap-publish-test.XXXXXX")
trap 'rm -rf -- "$work"' EXIT

tag=v0.1.0-beta.1
dist="$work/dist"
mock_bin="$work/bin"
log="$work/gh.log"
mkdir -p "$dist" "$mock_bin"
ln -s "$repo/scripts/test-publish-release.sh" "$mock_bin/gh"
ln -s "$repo/scripts/test-publish-release.sh" "$mock_bin/git"

expected_assets=$(
  printf '%s\n' \
    checksums.txt \
    "wrap_${tag}_darwin_amd64.tar.gz" \
    "wrap_${tag}_darwin_arm64.tar.gz" \
    "wrap_${tag}_linux_amd64.tar.gz" \
    "wrap_${tag}_linux_arm64.tar.gz" |
    LC_ALL=C sort
)
while IFS= read -r asset; do
  : >"$dist/$asset"
done <<<"$expected_assets"

run_publish() {
  PATH="$mock_bin:$PATH" \
    WRAP_MOCK_GH=1 \
    WRAP_MOCK_LOG="$log" \
    WRAP_MOCK_STATE="${WRAP_MOCK_STATE:-}" \
    WRAP_MOCK_ASSETS="${WRAP_MOCK_ASSETS:-}" \
    WRAP_MOCK_BODY="${WRAP_MOCK_BODY:-}" \
    WRAP_MOCK_UPLOAD_FAIL="${WRAP_MOCK_UPLOAD_FAIL:-0}" \
    WRAP_MOCK_GIT=1 \
    WRAP_MOCK_TAG_SHA="${WRAP_MOCK_TAG_SHA:-}" \
    WRAP_MOCK_TAG_OBJECT="${WRAP_MOCK_TAG_OBJECT:-}" \
    WRAP_MOCK_TAG_MOVE_AT="${WRAP_MOCK_TAG_MOVE_AT:-}" \
    WRAP_MOCK_MOVED_TAG_SHA="${WRAP_MOCK_MOVED_TAG_SHA:-}" \
    WRAP_MOCK_GIT_FETCH_FAIL_AT="${WRAP_MOCK_GIT_FETCH_FAIL_AT:-}" \
    WRAP_EXPECTED_SHA="${WRAP_EXPECTED_SHA:-}" \
    WRAP_EXPECTED_TAG_OBJECT="${WRAP_EXPECTED_TAG_OBJECT:-}" \
    "$publisher" "$tag" "$dist"
}

assert_log_has() {
  if ! grep -Fq -- "$1" "$log"; then
    echo "missing log entry $1:" >&2
    sed 's/^/  /' "$log" >&2
    exit 1
  fi
}

assert_log_lacks() {
  if grep -Fq -- "$1" "$log"; then
    echo "unexpected log entry $1:" >&2
    sed 's/^/  /' "$log" >&2
    exit 1
  fi
}

# A failed upload leaves a draft and never reaches publication.
: >"$log"
WRAP_MOCK_STATE=
WRAP_MOCK_ASSETS=
WRAP_MOCK_UPLOAD_FAIL=1
WRAP_EXPECTED_SHA=
WRAP_EXPECTED_TAG_OBJECT=
WRAP_MOCK_BODY=
if run_publish; then
  echo "interrupted upload unexpectedly succeeded" >&2
  exit 1
fi
assert_log_has "release create"
assert_log_has "release upload"
assert_log_lacks "release edit"

# A retry resumes the existing draft, uploads the complete set, then publishes.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_MOCK_UPLOAD_FAIL=0
WRAP_EXPECTED_SHA=expected
WRAP_EXPECTED_TAG_OBJECT=object
WRAP_MOCK_TAG_SHA=expected
WRAP_MOCK_TAG_OBJECT=object
WRAP_MOCK_TAG_MOVE_AT=
WRAP_MOCK_MOVED_TAG_SHA=
WRAP_MOCK_BODY="<!-- wrap-release-identity: expected object -->"
run_publish
assert_log_lacks "release create"
assert_log_has "release upload"
assert_log_has "release edit"
if [[ $(grep -Fc "git fetch" "$log") != 4 ]]; then
  echo "successful publication did not verify the remote tag four times:" >&2
  sed 's/^/  /' "$log" >&2
  exit 1
fi

# A published release is immutable to this workflow.
: >"$log"
WRAP_MOCK_STATE=false
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=
WRAP_EXPECTED_TAG_OBJECT=
WRAP_MOCK_BODY=
if run_publish; then
  echo "published release was modified" >&2
  exit 1
fi
assert_log_lacks "release upload"
assert_log_lacks "release edit"

# An administrator bypass that moves the tag in the final publication window
# is detected after publication and immediately returns the release to draft.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=expected
WRAP_EXPECTED_TAG_OBJECT=object
WRAP_MOCK_TAG_SHA=expected
WRAP_MOCK_TAG_OBJECT=object
WRAP_MOCK_TAG_MOVE_AT=4
WRAP_MOCK_MOVED_TAG_SHA=moved
WRAP_MOCK_BODY="<!-- wrap-release-identity: expected object -->"
if run_publish; then
  echo "tag move during publication unexpectedly succeeded" >&2
  exit 1
fi
assert_log_has "release edit $tag --draft=false"
assert_log_has "release edit $tag --draft=true"

# A failed defensive remote fetch is not allowed to trust the locally cached
# tag. Publication is rolled back until the remote identity can be checked.
: >"$log"
WRAP_MOCK_TAG_MOVE_AT=
WRAP_MOCK_GIT_FETCH_FAIL_AT=4
if run_publish; then
  echo "post-publication tag fetch failure unexpectedly succeeded" >&2
  exit 1
fi
assert_log_has "release edit $tag --draft=false"
assert_log_has "release edit $tag --draft=true"
WRAP_MOCK_GIT_FETCH_FAIL_AT=

# Even a successful upload call cannot publish an incomplete asset set.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS=checksums.txt
WRAP_EXPECTED_SHA=
WRAP_EXPECTED_TAG_OBJECT=
WRAP_MOCK_BODY=
if run_publish; then
  echo "incomplete asset set was published" >&2
  exit 1
fi
assert_log_has "release upload"
assert_log_lacks "release edit"

# A moved tag aborts before any stale assets are uploaded.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=expected
WRAP_EXPECTED_TAG_OBJECT=object
WRAP_MOCK_TAG_SHA=moved
WRAP_MOCK_TAG_OBJECT=object
WRAP_MOCK_TAG_MOVE_AT=
WRAP_MOCK_MOVED_TAG_SHA=
WRAP_MOCK_BODY=
if run_publish; then
  echo "moved tag was published" >&2
  exit 1
fi
assert_log_has "git fetch"
assert_log_lacks "release list"
assert_log_lacks "release upload"
assert_log_lacks "release edit"

# A hyphen inside SemVer build metadata does not make a stable release a
# prerelease. Only the version core before "+" controls classification.
original_tag=$tag
tag=v1.2.3+build-1
build_assets=$(
  printf '%s\n' \
    checksums.txt \
    "wrap_${tag}_darwin_amd64.tar.gz" \
    "wrap_${tag}_darwin_arm64.tar.gz" \
    "wrap_${tag}_linux_amd64.tar.gz" \
    "wrap_${tag}_linux_arm64.tar.gz" |
    LC_ALL=C sort
)
while IFS= read -r asset; do
  : >"$dist/$asset"
done <<<"$build_assets"
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$build_assets"
WRAP_EXPECTED_SHA=
WRAP_EXPECTED_TAG_OBJECT=
WRAP_MOCK_BODY=
run_publish
assert_log_has "release edit $tag --draft=false --prerelease=false"
tag=$original_tag

# A tag that moves after upload remains a draft and is never published.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=expected
WRAP_EXPECTED_TAG_OBJECT=object
WRAP_MOCK_TAG_SHA=expected
WRAP_MOCK_TAG_OBJECT=object
WRAP_MOCK_TAG_MOVE_AT=3
WRAP_MOCK_MOVED_TAG_SHA=moved
WRAP_MOCK_BODY="<!-- wrap-release-identity: expected object -->"
if run_publish; then
  echo "tag moved after upload was published" >&2
  exit 1
fi
assert_log_has "release upload"
assert_log_lacks "release edit"

# A draft created for an older tag target refreshes its notes before reuse.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=new
WRAP_EXPECTED_TAG_OBJECT=new-object
WRAP_MOCK_TAG_SHA=new
WRAP_MOCK_TAG_OBJECT=new-object
WRAP_MOCK_TAG_MOVE_AT=
WRAP_MOCK_MOVED_TAG_SHA=
WRAP_MOCK_BODY="<!-- wrap-release-identity: new old-object -->"
run_publish
assert_log_has "release edit $tag --notes-file"
assert_log_has "release edit $tag --draft=false"

# Replacing only the annotated tag object is still an identity change.
: >"$log"
WRAP_MOCK_STATE=true
WRAP_MOCK_ASSETS="$expected_assets"
WRAP_EXPECTED_SHA=expected
WRAP_EXPECTED_TAG_OBJECT=object
WRAP_MOCK_TAG_SHA=expected
WRAP_MOCK_TAG_OBJECT=reannotated
WRAP_MOCK_TAG_MOVE_AT=
WRAP_MOCK_MOVED_TAG_SHA=
WRAP_MOCK_BODY="<!-- wrap-release-identity: expected object -->"
if run_publish; then
  echo "replaced tag annotation was published" >&2
  exit 1
fi
assert_log_lacks "release list"
assert_log_lacks "release upload"
assert_log_lacks "release edit"
