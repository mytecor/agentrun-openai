#!/usr/bin/env bash
# Cross-compile release archives into dist/.
#
# Usage: scripts/build-release.sh [version]
#
# The version defaults to the current git description and is stamped into the
# binary, so `agentrun-openai --version` reports it.
set -euo pipefail

cd "$(dirname "$0")/.."

BINARY=agentrun-openai
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST=dist

# The agentrun engines are Unix-only; Windows does not compile.
PLATFORMS=(
	linux/amd64
	linux/arm64
	darwin/amd64
	darwin/arm64
)

rm -rf "$DIST"
mkdir -p "$DIST"

for platform in "${PLATFORMS[@]}"; do
	goos="${platform%/*}"
	goarch="${platform#*/}"
	stage="$DIST/${BINARY}_${VERSION}_${goos}_${goarch}"

	mkdir -p "$stage"
	echo "building $goos/$goarch"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=${VERSION}" \
		-o "$stage/$BINARY" \
		./cmd/"$BINARY"

	cp README.md "$stage/"
	tar -czf "${stage}.tar.gz" -C "$DIST" "$(basename "$stage")"
	rm -rf "$stage"
done

cd "$DIST"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum ./*.tar.gz | sed 's| \./| |' > SHA256SUMS
else
	shasum -a 256 ./*.tar.gz | sed 's| \./| |' > SHA256SUMS
fi
echo
echo "artifacts in $DIST:"
ls -1
