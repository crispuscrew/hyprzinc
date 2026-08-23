#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
fixture_root=$(mktemp -d)
output_dir="$fixture_root/release-assets"
trap 'rm -rf "$fixture_root"' EXIT HUP INT TERM

source_paths="
creator/bin/zc
container/runner/bin/zcr
launcher/gui/bin/zlg
launcher/tui/bin/zlt
virtualization/runner/bin/zvr
"

for source_path in $source_paths; do
	source_file="$fixture_root/$source_path"
	asset_name=${source_path##*/}
	mkdir -p "$(dirname "$source_file")"
	printf '#!/bin/sh\nprintf "%%s\\n" "%s v0.10.1"\n' "$asset_name" > "$source_file"
	chmod 0755 "$source_file"
done

SOURCE_ROOT="$fixture_root" sh "$repo_root/.github/scripts/package-release-assets.sh" \
	"$output_dir" v0.10.1

for asset_name in zc zcr zlg zlt zvr; do
	[ -x "$output_dir/$asset_name-linux-amd64" ] || {
		echo "missing executable release asset: $asset_name-linux-amd64" >&2
		exit 1
	}
done
(
	cd "$output_dir"
	sha256sum -c SHA256SUMS
)

printf '#!/bin/sh\nprintf "%%s\\n" "zc v0.10.0"\n' > "$fixture_root/creator/bin/zc"
if SOURCE_ROOT="$fixture_root" sh "$repo_root/.github/scripts/package-release-assets.sh" \
	"$fixture_root/wrong-version" v0.10.1 >/dev/null 2>&1; then
	echo "packaging accepted a binary with the wrong version" >&2
	exit 1
fi

echo "release asset packaging passed"

fake_bin="$fixture_root/fake-bin"
fake_log="$fixture_root/gh.log"
published_dir="$fixture_root/published-assets"
mkdir -p "$fake_bin" "$published_dir"
cp "$output_dir"/* "$published_dir/"
cat > "$fake_bin/gh" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$GH_LOG"
case "${1:-}:${2:-}" in
	release:view)
		[ "$RELEASE_STATE" != missing ] || exit 1
		printf '%s\n' "$RELEASE_STATE"
		;;
	release:download)
		download_dir=""
		while [ "$#" -gt 0 ]; do
			if [ "$1" = "--dir" ]; then
				download_dir=$2
				break
			fi
			shift
		done
		[ -n "$download_dir" ] || exit 2
		cp "$PUBLISHED_ASSETS"/* "$download_dir/"
		;;
esac
EOF
chmod 0755 "$fake_bin/gh"

: > "$fake_log"
GH_LOG="$fake_log" RELEASE_STATE=missing PUBLISHED_ASSETS="$published_dir" \
	PATH="$fake_bin:$PATH" sh "$repo_root/.github/scripts/publish-release-assets.sh" \
	"$output_dir" v0.10.1
grep -q "release create v0.10.1" "$fake_log"
grep -q -- "--draft --verify-tag" "$fake_log"
grep -q "release edit v0.10.1 --draft=false" "$fake_log"

: > "$fake_log"
GH_LOG="$fake_log" RELEASE_STATE=true PUBLISHED_ASSETS="$published_dir" \
	PATH="$fake_bin:$PATH" sh "$repo_root/.github/scripts/publish-release-assets.sh" \
	"$output_dir" v0.10.1
grep -q "release upload v0.10.1" "$fake_log"
grep -q -- "--clobber" "$fake_log"
grep -q "release edit v0.10.1 --draft=false" "$fake_log"

: > "$fake_log"
GH_LOG="$fake_log" RELEASE_STATE=false PUBLISHED_ASSETS="$published_dir" \
	PATH="$fake_bin:$PATH" sh "$repo_root/.github/scripts/publish-release-assets.sh" \
	"$output_dir" v0.10.1
grep -q "release download v0.10.1" "$fake_log"
if grep -q "release upload\|release edit" "$fake_log"; then
	echo "published release assets were mutated" >&2
	exit 1
fi

printf 'stale\n' > "$published_dir/unexpected-asset"
if GH_LOG="$fake_log" RELEASE_STATE=false PUBLISHED_ASSETS="$published_dir" \
	PATH="$fake_bin:$PATH" sh "$repo_root/.github/scripts/publish-release-assets.sh" \
	"$output_dir" v0.10.1 >/dev/null 2>&1; then
	echo "published release verification accepted an unexpected asset" >&2
	exit 1
fi
rm "$published_dir/unexpected-asset"

printf 'corrupt\n' >> "$published_dir/zc-linux-amd64"
if GH_LOG="$fake_log" RELEASE_STATE=false PUBLISHED_ASSETS="$published_dir" \
	PATH="$fake_bin:$PATH" sh "$repo_root/.github/scripts/publish-release-assets.sh" \
	"$output_dir" v0.10.1 >/dev/null 2>&1; then
	echo "published release verification accepted a corrupt asset" >&2
	exit 1
fi

echo "release publication state handling passed"
