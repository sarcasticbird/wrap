#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 <tag> <dist-directory>" >&2
  exit 2
}

[[ $# == 2 ]] || usage
tag=$1
dist_input=$2

case "$tag" in
  v[0-9]*)
    if [[ $tag == *[!A-Za-z0-9._+-]* ]]; then
      echo "invalid release tag: $tag" >&2
      exit 2
    fi
    ;;
  *)
    echo "release tag must start with v followed by a digit: $tag" >&2
    exit 2
    ;;
esac

if [[ ! -d $dist_input ]]; then
  echo "release directory does not exist: $dist_input" >&2
  exit 2
fi
dist=$(CDPATH= cd -- "$dist_input" && pwd)

assets=(
  "$dist/wrap_${tag}_darwin_amd64.tar.gz"
  "$dist/wrap_${tag}_darwin_arm64.tar.gz"
  "$dist/wrap_${tag}_linux_amd64.tar.gz"
  "$dist/wrap_${tag}_linux_arm64.tar.gz"
  "$dist/checksums.txt"
)
for asset in "${assets[@]}"; do
  if [[ ! -f $asset ]]; then
    echo "missing release asset: $asset" >&2
    exit 1
  fi
done

prerelease=false
version_core=${tag%%+*}
case "$version_core" in
  *-*) prerelease=true ;;
esac

verify_tag_unchanged() {
  if [[ -z ${WRAP_EXPECTED_SHA:-} ]]; then
    return
  fi
  if [[ -z ${WRAP_EXPECTED_TAG_OBJECT:-} ]]; then
    echo "WRAP_EXPECTED_TAG_OBJECT is required with WRAP_EXPECTED_SHA" >&2
    return 2
  fi
  git fetch --force origin "refs/tags/${tag}:refs/tags/${tag}" || return
  tag_object=$(git rev-parse "refs/tags/${tag}") || return
  tag_commit=$(git rev-list -n 1 "refs/tags/${tag}") || return
  if [[ $tag_commit != "$WRAP_EXPECTED_SHA" ]]; then
    echo "Tag $tag moved from expected commit $WRAP_EXPECTED_SHA to $tag_commit; refusing stale release publication" >&2
    return 1
  fi
  if [[ $tag_object != "$WRAP_EXPECTED_TAG_OBJECT" ]]; then
    echo "Tag $tag annotation changed from expected object $WRAP_EXPECTED_TAG_OBJECT to $tag_object; refusing stale release publication" >&2
    return 1
  fi
}

notes_file=
cleanup() {
  if [[ -n $notes_file ]]; then
    rm -f -- "$notes_file"
  fi
}
trap cleanup EXIT

prepare_release_notes() {
  if [[ -n $notes_file ]]; then
    return
  fi
  notes_file=$(mktemp "${TMPDIR:-/tmp}/wrap-release-notes.XXXXXX")
  git for-each-ref --format='%(contents)' "refs/tags/$tag" >"$notes_file"
  if [[ -n ${WRAP_EXPECTED_SHA:-} ]]; then
    printf '\n<!-- wrap-release-identity: %s %s -->\n' \
      "$WRAP_EXPECTED_SHA" "$WRAP_EXPECTED_TAG_OBJECT" >>"$notes_file"
  fi
}

# Asset uploads are separate API calls. Keep the release as a draft until
# every expected asset is present, and resume a draft left by an interrupted
# run so retries are safe.
verify_tag_unchanged
release_state=$(
  gh release list --limit 100 --json tagName,isDraft \
    --jq ".[] | select(.tagName == \"$tag\") | .isDraft"
)
case "$release_state" in
  "")
    prepare_release_notes
    gh release create "$tag" \
      --draft \
      --prerelease="$prerelease" \
      --verify-tag \
      --notes-file "$notes_file"
    ;;
  true)
    echo "Resuming existing draft release $tag"
    if [[ -n ${WRAP_EXPECTED_SHA:-} ]]; then
      release_body=$(gh release view "$tag" --json body --jq '.body')
      identity_marker="<!-- wrap-release-identity: $WRAP_EXPECTED_SHA $WRAP_EXPECTED_TAG_OBJECT -->"
      if [[ $release_body != *"$identity_marker"* ]]; then
        echo "Refreshing draft metadata for commit $WRAP_EXPECTED_SHA and tag object $WRAP_EXPECTED_TAG_OBJECT"
        prepare_release_notes
        gh release edit "$tag" --notes-file "$notes_file"
      fi
    fi
    ;;
  false)
    echo "Refusing to modify already-published release $tag" >&2
    exit 1
    ;;
  *)
    echo "Unexpected release state: $release_state" >&2
    exit 1
    ;;
esac

verify_tag_unchanged
gh release upload "$tag" --clobber "${assets[@]}"

expected_assets=$(
  for asset in "${assets[@]}"; do
    basename "$asset"
  done | LC_ALL=C sort
)
actual_assets=$(
  gh release view "$tag" --json assets --jq '.assets[].name' |
    LC_ALL=C sort
)
if [[ $actual_assets != "$expected_assets" ]]; then
  echo "Release remains a draft: uploaded asset set is incomplete" >&2
  diff -u \
    <(printf '%s\n' "$expected_assets") \
    <(printf '%s\n' "$actual_assets") || true
  exit 1
fi

verify_tag_unchanged
gh release edit "$tag" \
  --draft=false \
  --prerelease="$prerelease"

# The repository's v* tag ruleset makes release tags immutable. Recheck after
# publication as a defense in depth: if an administrator bypasses that rule in
# the final check-to-publish window, immediately return the release to draft.
if ! verify_tag_unchanged; then
  echo "Tag identity changed during publication; restoring draft status" >&2
  gh release edit "$tag" --draft=true
  exit 1
fi
