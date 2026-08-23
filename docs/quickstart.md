# Quick Start

This path installs the five v0.10.1 binaries on an existing Linux AMD64 system and starts
the smallest bundled container example. It needs rootless Podman, `curl`, `sha256sum`, and
a terminal emulator. GitHub release binaries are currently Linux AMD64 only; use the Nix
flake or build from source on AArch64.

## Install

These commands replace existing Zinc binaries in `~/.local/bin`.

```sh
(
  set -eu
  release=v0.10.1
  base_url="https://github.com/crispuscrew/zinc/releases/download/$release"
  download_dir="$(mktemp -d)"

  for binary_name in zc zcr zlg zlt zvr; do
    curl --fail --location \
      --output "$download_dir/$binary_name-linux-amd64" \
      "$base_url/$binary_name-linux-amd64"
  done
  curl --fail --location --output "$download_dir/SHA256SUMS" "$base_url/SHA256SUMS"
  (cd "$download_dir" && sha256sum --check SHA256SUMS)

  install -d "$HOME/.local/bin"
  for binary_name in zc zcr zlg zlt zvr; do
    install -m 0755 "$download_dir/$binary_name-linux-amd64" "$HOME/.local/bin/$binary_name"
  done
  "$HOME/.local/bin/zc" version
)
export PATH="$HOME/.local/bin:$PATH"
```

## Start An App

`zc init` writes examples without replacing existing app definitions. Zinc deliberately
uses `podman --pull never`, so pull the exact pinned image before the first launch.

```sh
podman info
zc init
podman pull docker.io/library/alpine@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1

# Use a terminal command installed on your system. TERMINAL is also accepted.
export ZINC_TERMINAL=foot

zc validate example-shell
zc run example-shell          # inspect the launch plan; this starts nothing
zc run example-shell --exec   # open the shell in the configured terminal
```

While the shell is open, `zcr ps` shows it. Close it normally or run
`zc stop example-shell` from another terminal.
