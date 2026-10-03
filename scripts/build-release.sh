#!/usr/bin/env bash
# Cross-compiles chronify for every supported platform and packages each
# binary with the README and LICENSE.
#
#   scripts/build-release.sh v1.0.0
#
# Output goes to dist/: one archive per platform (.tar.gz, or .zip for
# Windows) plus checksums.txt.
set -euo pipefail

version="${1:?usage: scripts/build-release.sh VERSION (e.g. v1.0.0)}"
targets=(
	darwin/amd64
	darwin/arm64
	linux/amd64
	linux/arm64
	windows/amd64
	windows/arm64
)

root="$(cd "$(dirname "$0")/.." && pwd)"
dist="$root/dist"
rm -rf "$dist"
mkdir -p "$dist"

for target in "${targets[@]}"; do
	os="${target%/*}"
	arch="${target#*/}"
	name="chronify_${version}_${os}_${arch}"
	stage="$dist/$name"
	binary="chronify"
	[ "$os" = windows ] && binary="chronify.exe"

	echo "Building $name"
	mkdir -p "$stage"
	(
		cd "$root"
		CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
			-trimpath \
			-ldflags "-s -w -X main.version=$version" \
			-o "$stage/$binary" .
	)
	cp "$root/README.md" "$root/LICENSE" "$stage/"

	if [ "$os" = windows ]; then
		(cd "$dist" && zip -qr "$name.zip" "$name")
	else
		tar -C "$dist" -czf "$dist/$name.tar.gz" "$name"
	fi
	rm -rf "$stage"
done

cd "$dist"
if command -v sha256sum >/dev/null; then
	sha256sum chronify_* >checksums.txt
else
	shasum -a 256 chronify_* >checksums.txt
fi
echo "Release files in $dist:"
ls -1 "$dist"
