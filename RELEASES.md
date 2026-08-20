# Zinc - Release Plan


| Version | Focus          | Includes              |
|---------|----------------|-----------------------|
| 0.1.0   | Containers     | `zc` mvp + `zcr` mvp  |
| 0.2.0   | Launcher       | `zlt` mvp             |
| 0.3.0   | Launcher       | `zlg` mvp             |
| 0.4.0   | Virtualization | `zvr` mvp (`zc` authors VM apps too) |
| 0.5.0   | Guest GPU      | Vulkan through venus + confirmed virgl |
| 0.6.0   | Windows guests | UEFI + Secure Boot + TPM, `zvr install`, per-app machine identity, fixed screen size, guest driver script |
| 0.7.0   | Containment    | resources + user enforced, sibling routing (`Via`/`Forward`/`ForwardPorts`), readiness gating, config inheritance, domain allowlists, compose interop, runner-built WireGuard tunnels |
| 0.8.0   | Session bus    | per-instance filtered D-Bus (`DBusMeta`) authored from CLI and TUI, Apache 2.0 licence, CI runner/runtime pinning |
| 0.8.1   | Packaging      | Nix flake + home-manager module, instance addressing and `zcr where` |
| 0.8.2   | Instances      | `zcr run --instance`, `{state}` mount templating, `zcr recheck` pin staleness, `zc init` |
| 0.9.0   | Attestable sandbox | real `wp_security_context_v1` per instance, bus attribution (`zcr bus`), nftables counters and posture (`zcr net`) |
| 0.9.1   | Audit fixes    | 22 defects from an audit: shell injection from a wg-quick file into the NET_ADMIN helper, a relaunch that tore down the running app, an additive nft load, an unfiltered tunnel input chain |
| 0.10.0  | schema v3, and every field enforced | audio per direction (PipeWire security context + permissions), Configs mounted, anonymous volumes, notification filtering, Env, ReadOnlyRootfs, RequireSecurityContext, guest egress control (`zvr net`), signed tags + `SHA256SUMS` |
| ...     |                |                       |

**0.10.0 is a minor bump, not a patch.** It changes the app-config schema, so every existing
config needs editing: the version line, and the audio and Configs blocks. Under semantic
versioning a change that invalidates what users already have on disk cannot be a patch, and
calling it 0.9.2 would tell people the upgrade is safe to take without reading anything. The
migration is in the changelog.

## Cutting a release

One command:

```
make -f release.mk tag VERSION=0.10.0 # signed, annotated tag
```

The tag is signed, and this refuses rather than falling back to an unsigned one: a release that
silently was not signed is worse than one that failed to be, because only the first is invisible.
It needs `git config user.signingkey` (with `gpg.format=ssh` for an SSH key).

Pushing the tag is what produces `SHA256SUMS`. A tag-gated CI job builds every tool in its pinned
container, records the bytes, and attaches the file to the release. It is deliberately not made by
whoever cuts the tag: a checksum written on the machine that also built the binaries proves only
that the machine agrees with itself. The build is reproducible - CI runs `make repro` for every
module on every push, which builds twice and asserts identical bytes - so anyone can rebuild and
compare against what CI published. That is the whole reason the number is worth having.

Version numbers live in three places that have to move together, or the flake job fails on the
one that did not: `flake.nix`, the assertion in `.github/workflows/ci.yml`, and the table above.

**ZDE** (the Zinc Desktop Environment, `zde-niri` / `zde-hypr`) is a separate project
layered on Zinc: it lives in its own repository with its own release plan. Only the Zinc
core and its tools (containers, launchers, virtualization) are released from here.

