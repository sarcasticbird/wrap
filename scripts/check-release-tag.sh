#!/bin/sh
set -eu

# Validates that a release tag is safe to publish and, on success, prints the
# tag object id on stdout. A tag qualifies only when it is annotated, points at
# the currently checked-out commit, and that commit already lives on the
# release branch. The last check is the important one: a tag on an unmerged
# branch would otherwise ship binaries built from history that never passed
# branch-protected CI or review. The release workflow calls this after fetching
# the tag and the branch; scripts/test-check-release-tag.sh covers every branch.

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 TAG_REF [MAIN_REF]" >&2
  exit 2
fi

tag_ref=$1
main_ref=${2:-origin/main}

tag_object=$(git rev-parse "$tag_ref")
if [ "$(git cat-file -t "$tag_object")" != tag ]; then
  echo "Release tags must be annotated" >&2
  exit 1
fi

tag_commit=$(git rev-list -n 1 "$tag_ref")
head_commit=$(git rev-parse HEAD)
if [ "$tag_commit" != "$head_commit" ]; then
  echo "Tag moved while this workflow was running; refusing a stale release" >&2
  exit 1
fi

if ! git merge-base --is-ancestor "$tag_commit" "$main_ref"; then
  echo "Release tag $tag_ref points at a commit that is not on $main_ref; refusing to release unreviewed history" >&2
  exit 1
fi

printf '%s\n' "$tag_object"
