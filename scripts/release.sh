#!/bin/sh
# Build Collie for macOS and Linux and publish a GitHub release.
#
#   scripts/release.sh v2026.1123.1          build, then create the release
#   scripts/release.sh v2026.1123.1 --dry    build only (into dist/)
#
# Tags are vYYYY.MMDD.N: the repository also carries upstream's v0.x tags,
# and a date-based version is always newer than those and never collides.
#
# install.sh downloads releases/latest/download/collie-<os>-<arch>, so the
# asset names below are part of the contract.
set -eu

version=${1:?usage: scripts/release.sh vX.Y.Z [--dry]}
dry=${2:-}
cd "$(dirname "$0")/.."

case "$version" in v[0-9]*) ;; *) echo "version must look like v0.1.0" >&2; exit 2 ;; esac
if [ -z "$dry" ] && [ -n "$(git status --porcelain)" ]; then
  echo "commit your changes first: a release is built from what is committed" >&2
  exit 1
fi

rm -rf dist && mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os=${target%/*} arch=${target#*/}
  out=dist/collie-$os-$arch
  echo "building $out"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out" .
done
(cd dist && shasum -a 256 collie-* > checksums.txt)
cat dist/checksums.txt

[ "$dry" = "--dry" ] && exit 0

git tag -a "$version" -m "Collie $version" 2>/dev/null || true
git push origin "$version"
gh release create "$version" dist/collie-* dist/checksums.txt --repo TaoSama/collie \
  --title "Collie $version" \
  --notes "安装：\`curl -fsSL https://taosama.github.io/collie/install.sh | sh\`（做 hub 加 \`-s -- hub\`，加入电脑见 README）。也可以直接下载下面对应平台的二进制。"
