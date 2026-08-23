#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: publish-release-assets.sh ASSET_DIR TAG" >&2
	exit 2
fi

asset_dir=$1
release_tag=$2
release_state=$(gh release view "$release_tag" --json isDraft --jq .isDraft 2>/dev/null || printf missing)

verify_assets() {
	verification_dir=$(mktemp -d "${RUNNER_TEMP:-/tmp}/zinc-release-assets.XXXXXX")
	remote_dir="$verification_dir/remote"
	mkdir -p "$remote_dir"
	trap 'rm -rf "$verification_dir"' EXIT HUP INT TERM
	gh release download "$release_tag" --dir "$remote_dir"
	for asset_path in "$asset_dir"/*; do
		printf '%s\n' "${asset_path##*/}"
	done | LC_ALL=C sort > "$verification_dir/expected"
	for asset_path in "$remote_dir"/*; do
		printf '%s\n' "${asset_path##*/}"
	done | LC_ALL=C sort > "$verification_dir/actual"
	cmp "$verification_dir/expected" "$verification_dir/actual"
	cmp "$asset_dir/SHA256SUMS" "$remote_dir/SHA256SUMS"
	(cd "$remote_dir" && sha256sum -c SHA256SUMS)
}

case "$release_state" in
	missing)
		gh release create "$release_tag" "$asset_dir"/* --draft --verify-tag \
			--title "$release_tag" --notes "See CHANGELOG.md for what is in this release."
		;;
	true)
		gh release upload "$release_tag" "$asset_dir"/* --clobber
		;;
	false)
		verify_assets
		echo "$release_tag is already published with the verified asset set"
		exit 0
		;;
	*)
		echo "unexpected release state for $release_tag: $release_state" >&2
		exit 1
		;;
esac

verify_assets
gh release edit "$release_tag" --draft=false
