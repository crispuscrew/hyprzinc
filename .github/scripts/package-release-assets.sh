#!/bin/sh
set -eu

if [ "$#" -ne 2 ]; then
	echo "usage: package-release-assets.sh OUTPUT_DIR EXPECTED_VERSION" >&2
	exit 2
fi

output_dir=$1
expected_version=$2
source_root=${SOURCE_ROOT:-.}
asset_names="zc-linux-amd64 zcr-linux-amd64 zlg-linux-amd64 zlt-linux-amd64 zvr-linux-amd64"

mkdir -p "$output_dir"
install -m 0755 "$source_root/creator/bin/zc" "$output_dir/zc-linux-amd64"
install -m 0755 "$source_root/container/runner/bin/zcr" "$output_dir/zcr-linux-amd64"
install -m 0755 "$source_root/launcher/gui/bin/zlg" "$output_dir/zlg-linux-amd64"
install -m 0755 "$source_root/launcher/tui/bin/zlt" "$output_dir/zlt-linux-amd64"
install -m 0755 "$source_root/virtualization/runner/bin/zvr" "$output_dir/zvr-linux-amd64"

(
	cd "$output_dir"
	: > SHA256SUMS
	for asset_name in $asset_names; do
		sha256sum "$asset_name" >> SHA256SUMS
	done
)

for asset_name in $asset_names; do
	reported_version=$("$output_dir/$asset_name" version)
	case "$reported_version" in
		*" $expected_version") ;;
		*)
			echo "$asset_name reported '$reported_version', expected $expected_version" >&2
			exit 1
			;;
	esac
done
