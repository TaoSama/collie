#!/bin/sh
# Build Collie for macOS and Linux and publish a GitHub release.
#
# Tags are vYYYY.MMDD.N: the repository also carries upstream's v0.x tags,
# and a date-based version is always newer than those and never collides.
#
# install.sh downloads releases/latest/download/collie-<os>-<arch>, so the
# asset names below are part of the contract.
set -eu

usage() {
  echo 'usage: scripts/release.sh VERSION [--dry|--publish]'
  echo '  --dry      build only (into dist/)'
  echo '  --publish  publish the verified dist/ assets without rebuilding'
  echo '  default    build and publish'
}

fail() {
  echo "$1" >&2
  exit 1
}

usage_error() {
  usage >&2
  exit 2
}

case "${1:-}" in -h|--help) usage; exit 0 ;; esac
[ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage_error
version=$1
shift
case "${1:-}" in
  --dry) set -- -d ;;
  --publish) set -- -p ;;
  --help) set -- -h ;;
esac
mode=release
if getopts ':dph' option; then
  case "$option" in
    d) mode=dry ;;
    p) mode=publish ;;
    h) usage; exit 0 ;;
    *) usage_error ;;
  esac
fi
shift "$((OPTIND - 1))"
[ "$#" -eq 0 ] || usage_error

cd "$(dirname "$0")/.."
case "$version" in v[0-9]*) ;; *) echo 'version must look like v0.1.0' >&2; exit 2 ;; esac
tag=refs/tags/$version
git check-ref-format "$tag" || fail "invalid release tag: $version"

set -- collie-darwin-amd64 collie-darwin-arm64 collie-linux-amd64 collie-linux-arm64
revision=$(git rev-parse HEAD)

require_clean_tree() {
  status=$(git status --porcelain)
  [ -z "$status" ] || fail 'commit your changes first: a release is built from what is committed'
}

build_assets() {
  rm -rf dist
  mkdir -p dist
  for asset do
    target=${asset#collie-}
    os=${target%-*} arch=${target##*-}
    echo "building dist/$asset"
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/$asset" .
  done
  (cd dist && shasum -a 256 "$@" > checksums.txt)
  printf '%s\n%s\n' "$version" "$revision" > dist/release.txt
  cat dist/checksums.txt
}

verify_assets() {
  if ! printf '%s\n%s\n' "$version" "$revision" | cmp -s - dist/release.txt; then
    fail "dist/ is missing a build for $version at $revision; build it with --dry first"
  fi
  checksums=$(cd dist && shasum -a 256 "$@")
  if ! printf '%s\n' "$checksums" | cmp -s - dist/checksums.txt; then
    fail 'dist/ assets no longer match checksums.txt'
  fi

  for asset do
    target=${asset#collie-}
    os=${target%-*} arch=${target##*-}
    if ! go version -m "dist/$asset" | awk -v rev="$revision" -v os="$os" -v arch="$arch" '
      $1 == "build" && $2 == "vcs.revision=" rev { revision_matches = 1 }
      $1 == "build" && $2 == "vcs.modified=false" { clean = 1 }
      $1 == "build" && $2 == "GOOS=" os { os_matches = 1 }
      $1 == "build" && $2 == "GOARCH=" arch { arch_matches = 1 }
      END { exit !(revision_matches && clean && os_matches && arch_matches) }
    '; then
      fail "dist/$asset was not built for $os/$arch from the clean commit $revision"
    fi
  done
}

if [ "$mode" != dry ]; then
  require_clean_tree
fi
if [ "$mode" != publish ]; then
  build_assets "$@"
fi
[ "$mode" = dry ] && exit 0

require_clean_tree
[ "$(git rev-parse HEAD)" = "$revision" ] || fail 'HEAD changed during the release'
verify_assets "$@"
if git show-ref --verify --quiet "$tag"; then
  [ "$(git rev-parse "$tag^{commit}")" = "$revision" ] || fail "tag $version points to a different commit"
else
  git tag -a "$version" "$revision" -m "Collie $version"
fi
git push origin "$tag"
(cd dist && gh release create "$version" "$@" checksums.txt --repo TaoSama/collie \
  --title "Collie $version" \
  --notes "安装：\`curl -fsSL https://collie.taoai.site/install.sh | sh\`（做 hub 加 \`-s -- hub\`，加入电脑见 README）。也可以直接下载下面对应平台的二进制。")
