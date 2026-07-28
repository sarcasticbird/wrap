#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 VERSION OUTPUT_DIR" >&2
  exit 2
fi

version=$1
output=$2

case "$version" in
  v[0-9]*) ;;
  *)
    echo "version must start with v followed by a digit" >&2
    exit 2
    ;;
esac
case "$version" in
  *[!A-Za-z0-9._+-]*)
    echo "version contains an unsafe filename character" >&2
    exit 2
    ;;
esac

repo=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
stage_root=$(mktemp -d "$output/.stage.XXXXXX")
trap 'rm -rf -- "$stage_root"' 0 1 2 15
umask 022

for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
  os=${target%/*}
  arch=${target#*/}
  name="wrap_${version}_${os}_${arch}"
  stage="$stage_root/${os}_${arch}"

  mkdir -p "$stage/docs" "$stage/examples"
  (
    cd "$repo"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
      -ldflags "-X main.version=${version}" \
      -o "$stage/wrap" ./cmd/wrap
  )
  cp "$repo/LICENSE" "$repo/README.md" "$repo/SECURITY.md" "$stage/"
  cp "$repo/docs/architecture.md" "$repo/docs/configuration.md" \
    "$repo/docs/troubleshooting.md" "$stage/docs/"
  cp "$repo/examples/wrap.toml" "$stage/examples/"
  chmod 0755 "$stage/wrap"
  chmod 0644 "$stage/LICENSE" "$stage/README.md" "$stage/SECURITY.md" \
    "$stage/docs/architecture.md" "$stage/docs/configuration.md" \
    "$stage/docs/troubleshooting.md" "$stage/examples/wrap.toml"
  TZ=UTC touch -t 197001010000 "$stage/wrap" "$stage/LICENSE" \
    "$stage/README.md" "$stage/SECURITY.md" "$stage/docs/architecture.md" \
    "$stage/docs/configuration.md" "$stage/docs/troubleshooting.md" \
    "$stage/examples/wrap.toml"
  case "$(tar --version 2>/dev/null)" in
    *bsdtar*)
      COPYFILE_DISABLE=1 tar --uid 0 --gid 0 --numeric-owner \
        --options gzip:!timestamp -C "$stage" \
        -czf "$output/${name}.tar.gz" wrap LICENSE README.md SECURITY.md \
        docs/architecture.md docs/configuration.md docs/troubleshooting.md \
        examples/wrap.toml
      ;;
    *)
      tar --sort=name --mtime="@0" --owner=0 --group=0 --numeric-owner \
        -C "$stage" -czf "$output/${name}.tar.gz" \
        wrap LICENSE README.md SECURITY.md docs/architecture.md \
        docs/configuration.md docs/troubleshooting.md examples/wrap.toml
      ;;
  esac
done

(
  cd "$output"
  shasum -a 256 "wrap_${version}"_*.tar.gz > checksums.txt
)

rm -rf -- "$stage_root"
trap - 0 1 2 15
