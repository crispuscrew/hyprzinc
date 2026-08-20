# Zinc - Architecture

> **Priority order: Stable, then Secure, then Beautiful.**
> Keyboard-first, no mouse required. Zinc is a security-focused sandboxing core: it runs
> user-facing apps in rootless Podman containers, each walled off from the rest of the
> desktop, on any Linux distribution (Fedora is the primary development target).

This document is the single source of truth for what Zinc actually ships. Section numbers
are cited from the code (for example "architecture.md 5.3" points at the network model), so
the numbering is kept stable. It targets the **0.1 release**, which ships two tools: `zc`
(the container creator) and `zcr` (the container runner). Everything marked *planned* is on
the roadmap and does not ship yet.

---

## 1. Design Principles

- **Stable first** - declarative config, pinned versions, reproducible from scratch on any
  machine.
- **Secure by default** - minimum host surface; a container gets nothing unless the config
  explicitly grants it. Fail-closed: anything the runtime cannot enforce is rejected, not
  run.
- **Beautiful always** - a consistent visual language, but never at the cost of the two
  priorities above.
- **Keyboard sovereign** - every interaction is reachable without a mouse.
- **No magic** - every decision is explicit, documented, and reversible. A dry run prints
  the exact `podman` commands and firewall rules before anything executes.
- **Honest about limits** - where a security mechanism is partial (see 5.2), the doc says so
  loudly rather than marketing it as more than it is.

---

## 2. Stack Overview

Zinc is the sandboxing core. It is compositor-agnostic and installs cleanly on top of an
existing system; **ZDE** (the Zinc Desktop Environment) is a separate project layered on top
and is out of scope here.

```
+-------------------------------------------------------------+
|  Any Wayland compositor                                     |
|  + wayland-security-context (a real per-instance context    |
|    where the compositor has it; see 5.2)                    |
+-----------------------------+-------------------------------+
|  zc - zinc-creator (CLI + Bubbletea TUI, Go)     |
|    authors app files, forwards run/manage to zcr            |
|                          | shells out over $PATH            |
|  zcr - zinc-container-runner (runtime, Go)                  |
|    reads an app file, runs it, locks the network down       |
+-----------------------------+-------------------------------+
|  Config store (YAML)                                        |
|    ~/.config/zinc/apps/<name>.yaml   (app definitions)      |
|    ~/.config/zinc/zc                (zc's TUI keybinds)   |
+-----------------------------+-------------------------------+
|  Rootless Podman + pasta networking                         |
|    per-app network namespace (a pod), locked by an nftables |
|    ruleset before the app starts (5.3); the ruleset is      |
|    applied by a small digest-pinned netfilter helper image  |
+-----------------------------+-------------------------------+
|  Any Linux distribution (Fedora is the primary dev target)  |
+-------------------------------------------------------------+
```

There is no daemon, no host firewall change, and no persistent background service. `zc` and
`zcr` are two static binaries you put on `$PATH`.

---

## 3. App Config (YAML)

One YAML file per app: `~/.config/zinc/apps/<name>.yaml`. The format is **schema version 3**.
The same file is validated identically at author time (in `zc`, on save) and at launch time
(in `zcr`, before anything runs), because the validation is pure and shared - so a manual
edit or drift cannot slip an invalid config past launch.

`AppConfig` is a flat struct with grouped `*Meta` sub-structs. There are no presets and no
network "modes"; behavior is exactly the fields below.

```yaml
SchemaVersion: 2                 # must be 2 (the only version this build understands)
Type: ZincContainer              # ZincContainer today (ZincVirtualization is planned)

AppNameID: firefox               # also the container/pod name; [a-z0-9._-], starts alphanumeric
Inherits: ""                     # optional: another app this one starts from (see 3.1)
Icon: firefox
Description: Web browser

StartConditions:
  DependsOn: []                  # other apps that must be up first (auto-started, see 6.6)
  ReadyCheck: []                 # exec-form probe deciding this app is ready for its dependents
  ReadyTimeoutSec: 0             # how long a dependent waits for it; 0 = 60s (see 6.6)
  Autorestart: false             # restart only on failure (a clean exit / manual stop is final)
  Entrypoint: firefox            # process to run; empty = the image's default command
  Terminal: false                # CLI/TUI app: launch in a host terminal-emulator window
  Multiterminal: false           # many terminals attach to one shared container (see 9.1)
  MultiterminalEntrypoint: ""    # per-terminal command; empty = Entrypoint

StopConditions:
  KeepAlive: false               # keep the container after the entrypoint exits (no --rm)
  Background: false              # stay alive after the window closes

ResourcesMeta:                   # enforced (>= 0; 0 = unlimited)
  MaxCPUCores: 2                 # fractional allowed (0.5)
  MaxRamMiB: 2048
  MaxSwapMiB: 0                  # on top of MaxRamMiB, which must be set alongside it
  PIDsLimit: 512                 # fork-bomb guard

InternalUserMeta:                # enforced
  UseNonRootUser: true
  NonRootUserName: app
  KeepUserID: false              # --userns=keep-id: the container sees the host's uid

ImageMeta:
  Image: docker.io/library/alpine@sha256:...  # third-party images MUST be digest-pinned (5.5)
  Install:                       # optional shell lines; if set, Zinc builds a derived image (5.5, 7)
    - apk add --no-cache firefox font-dejavu

DisplayMeta:
  DisableSecurityContext: false  # false = the app gets its own wp_security_context_v1 socket (5.2)
  RequireSecurityContext: false  # true = refuse to launch rather than fall back to the raw socket
  DisableGpuAccess: true         # true = no /dev/dri (GPU is granted unless this is set, 5.4)

NetworkMeta:
  DNSServers: []                 # resolvers the app may use, and the only ones it may reach
  NetworkLists: []               # empty = isolated (own localhost only). See 5.3 and section 6.
    # an egress list may name destinations by address and/or by name:
    #   IPv4CIDR / IPv6CIDR      # addresses, as written
    #   Domains                  # resolved AT LAUNCH into addresses; a snapshot, not name
    #                            # filtering, and not refreshed while the app runs (6.2)

NotificationMeta:                # needs DBusMeta.Talk to reach org.freedesktop.Notifications
  Disabled: false                # true = the call is refused, and the app is told
  Silenced: false                # true = accepted and dropped; the app sees success
  UseCustomPrefix: false
  CustomPrefix: ""               # prepended to every summary
  AllowedActions: false          # false = the action buttons are stripped
  AllowedProlonged: false        # false = expire_timeout is clamped to 10s
  AllowedLinks: false            # false = anchor markup is stripped from the body

Env:                             # the app's environment; Zinc's own variables are refused
  LANG: en_US.UTF-8
ReadOnlyRootfs: false            # true = --read-only (podman keeps /tmp, /run, /dev writable)

Configs:                         # files the app ships with, from apps/<app>/configs/
  - BundlePath: settings.json    # relative to the bundle; absolute is a Volume, not this
    InnerMount: /etc/app/settings.json
    Writable: false              # read-only by default
Volumes: []                      # explicit host bind mounts are wired; see below
Keys: []                         # SSH/GPG convenience mounts; see below
HostTheme: true                  # mount the curated host theme bundle read-only (5.6)
AudioMeta:                       # one grant per capability; absent means none
  Playback: default              # none | default | a list of /dev/snd nodes
  Microphone: none               # same three forms
  Monitor: none                  # record what OTHER apps play; none | default only
Capabilities: []                 # extra `--cap-add` entries, on top of the drop-all baseline
```

**Volumes.** Each `Volume` is explicit; there is no implicit home access. A volume with
`HostMounted: true` and a `HostMount` path is a bind mount: `HostMount:InnerMount`, with
`ro`/`rw` from `Writable` and `noexec`/`exec` from `Executable`.

A volume with no host path is scratch space rather than a location on the host, and is
mounted as a tmpfs at `InnerMount`, always `nosuid,nodev`, with the same `Writable` and
`Executable` defaults. `SizeLimited` with `SizeLimitMiB` becomes `tmpfs-size`, so the ceiling
is held by the kernel: a container writing past it gets ENOSPC. Left off, the size is
podman's default of half the host's RAM.

A bind mount can also be added for a single run without editing the app file, via a
repeatable `-v`/`--volume` flag on `zcr run`:
`zcr run <app> -v HOST:CONTAINER[:OPTIONS]`, where `OPTIONS` is a comma list (`rw`, `ro`,
`exec`, `noexec`; default `ro,noexec`). Such a mount is appended to the loaded config in
memory - never written back to the YAML - and passes through the same validation and
arg-builder as a configured `Volume`, so it is screened by the same field-shift guards.

```yaml
Volumes:
  - HostMounted: true
    HostMount: /home/user/Downloads
    InnerMount: /home/user/Downloads
    Writable: true
    Executable: false
```

**Keys.** A convenience layer for SSH/GPG only: unlike a plain volume, a `Key` mounts the key
read-only into the container home (`.ssh` for `SSH`, `.gnupg` for `GPG`). Per-key explicit
opt-in.

```yaml
Keys:
  - Type: SSH
    Path: /home/user/.ssh/id_ed25519
```

**Wired at runtime in 0.1:** identity/image, the network attach and lock-down, the
capability drop-all baseline plus `Capabilities`, the Wayland socket (its own, under a
security context, where the compositor implements one - 5.2),
GPU device, the theme bundle, audio (the PipeWire socket, or named `/dev/snd` nodes), explicit host bind
mounts, SSH/GPG key mounts, the entrypoint override, and the terminal / multiterminal /
background / keep-alive lifecycle. `ResourcesMeta` (`--cpus`, `--memory`, `--memory-swap`, `--pids-limit`) and
`InternalUserMeta` (`--user`, `--userns=keep-id`) are enforced now, and so is
`NotificationMeta` (see below). **Every field in the schema is wired into the launch.**

`Configs` was the one exception to that rule until schema v3: it validated, expanded
placeholders, was counted by `zc` and refused for VM apps, and then produced no mount, so an
app started without its file and nothing said why. It is wired now, and it has its own type
rather than borrowing `Volume` - which mattered because the borrowed `HostMount` carried the
opposite rule in each list (a Volume's must be absolute, a Config's must not be) while
`HostMounted`, `SizeLimited` and `SizeLimitMiB` could never apply to a single file at all.

---


### 3.1 Inheritance

`Inherits` names another app in the store this one starts from. The child states only what
differs and takes the rest from its base, which cuts the duplication across a family of
similar apps down to the part that is actually different.

**Resolution is live.** The merge happens every time a config is read, so editing a base
changes every app built on it at the next launch. That is the point of the feature and also
the thing to be careful with: a base that grants a capability grants it to every child, and
the child's own file will not say so. `zc validate <app> --resolved` prints exactly what an
app merges to, which is how an inheriting app is audited.

**The merge is on the YAML, not on the decoded struct**, and that is the whole design. Only
the text records which keys a config actually STATED: once decoded, `HostTheme: false` is
indistinguishable from an absent `HostTheme`, and an empty `Volumes` from an omitted one.
Merging structs would therefore have to read every zero value as "inherit", which means a
child could never turn a base's flag off and never empty a base's list - so a base could
loosen containment in a way no child could walk back. Merging nodes has no such rule.

The rules that fall out:

- A key the child states wins, whatever its value - `false` included, and an empty list
  included.
- A key the child omits comes from the base.
- Nested blocks merge field by field, so a child restating one field of `ResourcesMeta` keeps
  the base's other fields rather than replacing the block.
- A list the child states **replaces** the base's. Lists are not appended: a child could then
  never remove an inherited volume or capability, and capabilities that only ever accumulate
  down a chain are the wrong direction for this tool.
- A cycle, a missing base, or a chain deeper than 8 fails the read. A config that cannot be
  fully resolved is not run, since what it is missing could be the part that contains it.
- `Inherits` is charset-checked before any file is opened - the name is joined into a store
  path, so `../..` would otherwise read a config from outside the apps directory.

**What is stored is always what was written.** Every store exposes both reads: `Load` returns
the file as authored, `LoadResolved` returns what the app is. Launching, validating and
listing use the resolved form; anything that will write the config back uses the raw one.
Saving a resolved config over its source would flatten the inheritance the first time anyone
touched the app.

For the same reason `zc` **refuses to save an app that inherits**: a form knows the app's
values but not which of them it stated, so writing them all back would replace everything the
child inherits with zeros - silently, in a file that looks entirely normal afterwards. An
inheriting app is edited as a file. That is a real limit of combining struct-based editors
with a text-level merge, and refusing is the only version of it that cannot lose an author's
work.

## 4. Tools and the creator / runner split

Every Zinc tool is named `zinc-<kind>-<role>`, where `<kind>` is `container` or
`virtualization` and `<role>` is `runner`, plus a `zinc-launcher-<ui>` picker. One creator
authors both app kinds, so it carries no kind at all. The short code is the initials.

| Short | Tool                          | Role                                   | Status  |
|-------|-------------------------------|----------------------------------------|---------|
| `zc`  | `zinc-creator`                | define apps, container or VM           | 0.1     |
| `zcr` | `zinc-container-runner`       | launch + supervise a container app     | 0.1     |
| `zvr` | `zinc-virtualization-runner`  | launch + supervise a VM app            | 0.4     |
| `zlg` | `zinc-launcher-gui`           | fast app launcher (GUI)                | 0.3     |
| `zlt` | `zinc-launcher-tui`           | fast app launcher (TUI)                | 0.2     |

**The split is architectural, not cosmetic.** A *creator* authors an app and writes its
config; a *runner* actually starts that app and owns its lifecycle. `zc` (the creator)
depends only on the shared `common` library and **knows nothing about podman**. To run what
it authors, it shells out to the `zcr` binary on `$PATH`. The two meet only at the on-disk
YAML format and at that process boundary; `zc` never imports `zcr`.

```
   author / manage                        run / supervise
   +-----------------+   shells out to    +------------------+
   |  zc (creator)  |  ---- $PATH ---->  |  zcr (runner)    |
   |  depends: common|   run/build/stop   |  depends: common |
   |  no podman code |   logs/term/image  |  drives podman   |
   +-----------------+                    +------------------+
             \                                   /
              \______ on-disk YAML app file ____/
                 ~/.config/zinc/apps/<name>.yaml
```

`zc` authoring commands (`new`, `list`, `validate`, `delete`, `tui`, `keys`) work without
`zcr`. The runtime commands (`run`, `build`, `stop`, `restart`, `inspect`, `logs`, `term`,
`image`) are forwarded verbatim to `zcr`, streaming its output and preserving its exit
status; if `zcr` is missing, those commands fail with an actionable message while authoring
keeps working. Details in section 9.

---

## 5. Security Model - what each layer actually gives you

### 5.1 Container isolation (rootless Podman)

**Strong. This is the real security boundary.** Namespaces isolate PID, network, mounts,
IPC, and UTS. The user namespace maps container root to your unprivileged host user, so
"root in the container" is not root on the host. An app cannot see the host filesystem
outside its explicit mounts, cannot see other containers, and cannot escalate.

Every app container starts from a least-privilege baseline: `--security-opt
no-new-privileges --cap-drop all`. Anything the app genuinely needs is re-added explicitly
from `Capabilities`, and each capability is validated against a safe charset. The launch is
hermetic - `--pull never` - so a run never triggers a surprise registry pull; the image must
already be in local storage (resolved at author time or built as a derived image).

### 5.2 Wayland isolation (wayland-security-context)

**Real, and bounded by what the compositor does with it.** An app that gets the compositor's
own socket is indistinguishable from the desktop's own clients: it can ask for the same
protocols, and nothing at the other end knows it came out of a sandbox. `wp_security_context_v1`
is how that is fixed, and the fix is not a label on the container - it is a second socket.

Zinc connects to the **real** compositor socket, binds a socket of its own, and asks the
compositor to accept connections on it under a security context carrying three strings. The
app container mounts that second socket at the same container-side path and with the same
`WAYLAND_DISPLAY`, so nothing inside the app changes. What changes is that every connection
arriving on it is tagged before the app exists, by a process the app is not in, over a socket
it never held.

Two guarantees in the protocol are what make this shape possible. `listen_fd` must already be
bound and listening when `create_listener` is sent, so the socket is on disk before podman is
asked to bind-mount it. And the compositor must keep accepting on that socket after the client
which created the context disconnects, so the Wayland connection is a one-shot - opened, used,
closed - and the only thing that has to outlive it is a pipe.

The three identifiers are chosen so a desktop can cross-check them rather than take them on
faith:

- **`sandbox_engine`** is `com.github.crispuscrew.zinc`. Reverse-DNS as the protocol asks, and
  the repository's own domain rather than a tidy generic name: `app_id` is only required to be
  unique *per engine*, so the engine name is half the identity and has to be one this project
  actually holds.
- **`app_id`** is the app half of the address. The protocol requires it to be the same string
  across restarts and across instances of one application, which is what an app name is.
- **`instance_id`** is the runtime name - the same string that names the podman container and
  that `zcr where` reports. Deliberately not a random uuid: a uuid would satisfy the protocol
  and tell a consumer nothing, whereas this one can be looked up, so a compositor holding an
  `instance_id` can find the container behind the window.

**Lifetime.** `zcr run` detaches and exits, and a security context is revoked by closing a
descriptor - so something has to still be holding it. That is a hidden `zcr __wayland
<app[@instance]>` process, one per app instance, the same answer the multiterminal path
reached for the same reason (9.1). It binds the socket, performs the exchange, reports back to
the launch on a pipe, and then blocks until the app container is gone; on the way out it closes
`close_fd`, which is what tells the compositor to stop accepting, and removes the socket. The
launch waits for that report before creating the container, because the socket is a bind-mount
source and podman cannot mount a path that is not there.

**When the compositor does not implement it**, the app is given the compositor's own socket
and a warning is printed. Refusing to launch would buy nothing - the app would have had the
raw socket anyway - and would make Zinc unusable on a mainstream desktop that has not adopted
the protocol. Every other failure (a socket that cannot be bound, a compositor that cannot be
reached, a rejected request) fails the launch instead, because those are broken environments
rather than an absent feature, and a security context that is silently best-effort is one
nobody can rely on.

The container label says which of the two actually happened: `zinc.wayland=security-context`
or `zinc.wayland=passthrough`. It used to be applied whenever the app had not opted out, which
claimed a context on every launch including the ones that never created one - and a label that
is wrong is worse than no label, since it is exactly what a desktop would read to decide how
much to trust a client. `DisplayMeta.DisableSecurityContext` remains the opt-out and now means
what it says: mount the compositor's socket, create nothing.

**What this still does not do.** What a tagged connection is *allowed* to do is entirely the
compositor's policy - Zinc supplies the identity, not the rules - and compositors currently
restrict little beyond withholding the privileged protocols (screencopy, input inhibition, the
security context manager itself, so nesting is impossible). An app that never speaks the
protocols it is denied is unaffected by any of it. **The real isolation boundary for a
container is still the container itself** (5.1); for genuinely untrusted GUI apps the stronger
answer is a VM (section 10).

**Requiring it.** The fallback above is a real downgrade: the app becomes a client the
compositor cannot tell apart from an unsandboxed one, and the container is labelled
`zinc.wayland=passthrough` to record that. `DisplayMeta.RequireSecurityContext: true` refuses
the launch instead, naming the compositor as the reason. It exists for the app whose whole
reason for being sandboxed is that it is untrusted, where running it unlabelled is worse than
not running it. The default stays permissive because most compositors still lack the protocol
and a desktop that refused to start anything would not be one anyone runs. Setting it together
with `DisableSecurityContext` is refused rather than resolved: they are opposites.

### 5.3 Network isolation (per-app netns, fail-closed)

**Strong, and the crown jewel of the security model.** An app's `NetworkMeta.NetworkLists`
drive a fail-closed firewall applied in the app's **own network namespace before the app
process starts**. There is never an unfiltered window.

Mechanics:

- A filtered app (one with any `NetworkLists`) runs inside a **pod**. The pod's infra
  container owns the network namespace and provides connectivity via **pasta** (userspace,
  no host root, no host firewall change).
- Before the app container joins the pod, an **nftables ruleset is loaded into the pod's
  netns** by a one-shot init step: `podman run --pod <pod> --pull never --cap-drop all
  --cap-add NET_ADMIN --security-opt no-new-privileges <netfilter-image> nft -f -`, with the
  ruleset piped in on stdin. `CAP_NET_ADMIN` is namespaced to the pod's user namespace, so it
  grants nothing on the host.
- Only then is the app container started with `--pod <pod>`. The lock is in place first, so
  the app never sees an open network.
- An app with **no** `NetworkLists` gets `--network none`: it reaches only its own localhost,
  fully isolated.
- The whole thing is fail-closed. If any prepare step fails, the half-built pod is torn down.
  If the app dies after fork, a reaping goroutine tears the pod (and its netns) down too, so
  no rule-less netns is ever left behind.

Enforcement is a **port** in the runner hexagon (`NetEnforcer`, see section 13): the
pasta-plus-nft implementation is one adapter. Swapping the traffic-control mechanism later (a
different firewall, an eBPF egress filter, an external controller) is a new adapter, with the
launch path unchanged. The tiers and the exact ruleset are in section 6.

**Asking what it did.** A lock-down nobody can see the effect of is one nobody can trust, so
`zcr net` answers the two questions from outside:

- `zcr net` lists every **running** app with its network posture and, for a filtered one, the
  pod that owns its netns. Two postures, never merged: `filtered` (has `NetworkLists`, so a
  netns of its own with the ruleset locked in it) and `isolated` (no lists, so `--network
  none` - it reaches only its own localhost and has no netns or ruleset at all). They are
  separated because an isolated app is not a filtered app whose counters happen to be zero,
  and a table that showed them alike would say the opposite of what is true about one of them.
  Apps are named the way a person types them (`app@instance`), never the runtime form
  (`app.instance`); anything running that is not a defined app - Zinc's own D-Bus proxies, a
  container the user started - is left out rather than listed with a posture Zinc cannot vouch
  for.
- `zcr net <app[@instance]>` reports what that app's ruleset has actually counted: per rule,
  the chain, the verdict, the rule's label, packets and bytes. `--json` gives either as a
  machine-readable document for a desktop shell.

The read goes through the same helper image, the same pod and the same single capability as
the step that applied the ruleset - `nft -j list ruleset` instead of `nft -f -`. Reading
nftables is not a lesser privilege than writing it (one netlink socket, `CAP_NET_ADMIN`
either way), so there is no weaker path to take. It differs from the other helpers in one
respect: it runs while the app is alive, where they had all exited before it started. It
joins the pod's **network** namespace and nothing else - a podman pod shares net, ipc and
uts, not pid - so it cannot see, signal or ptrace the app, and it exits as soon as the dump
is written.

**A counter is not a lifetime total.** Counters live in the pod's netns and are created with
it, so they start at zero every time the pod is, which `zcr stop`, `zcr restart` and any
failed launch all do. The number means "since this launch"; nothing persists one, and nothing
is meant to. The output says so in both forms rather than leaving it to be assumed.

### 5.4 GPU passthrough

**Weak isolation when enabled.** Granting `/dev/dri` exposes GPU rendering state more
broadly than process boundaries suggest, and Linux GPU sandboxing is immature.

**GPU access is ON unless a config opts out** with `DisplayMeta.DisableGpuAccess: true`. An
app that says nothing about the display receives `/dev/dri`. This section claimed the
opposite until the schema v3 release; the code was always the authority and the text was wrong.

This is a deliberate exception, and the only grant in the schema whose zero value is the
permissive one. Every other grant defaults closed: no `NetworkLists` means `--network none`,
an empty `DBusMeta` means no bus at all, no `Capabilities` means `--cap-drop all`. GPU access
is opt-out because almost every graphical app needs it and a desktop whose apps render on the
CPU by default is not one anyone would run. The cost is that a reviewer scanning a config for
what it was granted will not find this one written down anywhere.

Rule: set `DisableGpuAccess: true` for anything that does not render, and never leave it on
for untrusted code.

**There is no VRAM limit, and this is why.** GPU memory is the one resource `ResourcesMeta`
cannot bound: an app granted `/dev/dri` can allocate until the device is exhausted, which is a
denial of service against the whole desktop rather than against itself. The mechanism that
would fix this is the kernel's `dmem` cgroup controller (Linux 6.14+), which accounts device
memory per cgroup and is exactly the right granularity. It is not usable yet:

- The controller has to be present AND the DRM driver has to register regions for it. On the
  development box here `dmem` appears in `cgroup.controllers`, and `dmem.capacity` is empty
  with no `dmem.max`, because the driver registers nothing. Most drivers still do not.
- Podman can set unified cgroup values with `--cgroup-conf`, so the plumbing on Zinc's side is
  small once regions exist.

So a `VramLimitMiB` field would read as a cap and do nothing on nearly every machine, which is
the failure this project refuses elsewhere. Deferred deliberately, to be added when the
controller is usable: it would be additive, with no schema bump, since absent means unlimited.

The VM side has two numbers that look like VRAM and are not. `hostmem` on `virtio-gpu-gl-pci`
is the address-space window venus shares blob resources through, and reserves address space
rather than committing memory; `vgamem` on `bochs-display` is emulated VGA memory sized from
the requested resolution, so exposing it would mainly let a config contradict its own
`DisplayWidth`/`DisplayHeight`. Neither is a memory cap, and neither is worth a field.

Until then the containment answer is the blunt one: deny the GPU to anything that does not
need it.

### 5.5 Image trust (digest pinning + derived images)

**Pin by digest, not by tag, for third-party images.** Validation enforces this: an
`ImageMeta.Image` that is not a `localhost/` reference must be a canonical digest pin
(`...@sha256:` followed by exactly 64 hex characters). Only `localhost/` images - which
resolve to local storage and can never pull something remote - may use a mutable tag. The
image reference must also be a single clean line (no whitespace or control characters),
because it is interpolated into a `FROM` line and into podman arguments.

To make pinning painless without a browser, `zc image search <term>` and `zc image resolve
<ref>` (forwarded to `zcr`) find an image and print its digest-pinned form to paste into
`ImageMeta.Image`; the TUI resolves the image field in place.

**Derived images** are the "quick setup" path and are covered in section 7. Their base
inherits the digest-pin rule above, so a derived build always starts from a known base.

### 5.6 Theme passthrough

**Default on, security impact minimal.** When `HostTheme` is true and a theme bundle is
available, the runner mounts a single curated directory read-only at `/etc/zinc/theme` inside
the container - not the host's real `~/.config` or `~/.themes`. The bundle path comes from
`ZINC_THEME_BUNDLE`; generating that bundle (GTK/Qt configs, icon and cursor themes, fonts)
is a job for the desktop layer (ZDE), out of scope for Zinc 0.1. The point of default-on is
that containerized apps look like part of the system; set `HostTheme: false` to deny it.

### 5.7 Session bus isolation (per-instance filtered D-Bus)

**Default off, and the default is the whole point.** The host session bus is a desktop-wide
capability: an app holding that socket can read the keyring, drive the portal, talk to the
compositor and address every other service the user runs. Mounting it into a sandbox opens a
hole the width of the desktop, so Zinc's default is that a container gets **no session bus at
all**.

`DBusMeta` opens exactly as much as it names, and nothing more:

- `Talk` - well-known names the app may send method calls to. A trailing `.*` matches a
  subtree, which also grants services that appear under it later, so an exact name is
  preferred.
- `Own` - names the app may claim, which is what a notifier, a tray icon or an MPRIS player
  needs to be addressable. A separate grant from `Talk` rather than implied by it, and a
  wildcard cannot be owned, since a process claims one concrete name or none.

The mechanism mirrors 5.3: the dangerous thing is established outside the app, and the app is
handed only the filtered result. `xdg-dbus-proxy` runs in the vetted helper image, in a
container Zinc owns, holding the real bus socket and serving a per-app filtered one. Three
properties carry the isolation:

1. **The proxy is not in the app's pod.** A pod shares the PID namespace, so a proxy inside it
   would be a process the app could signal or ptrace: the filter and the filtered, in one
   blast radius. They share exactly one thing, the socket, through a bind mount.
2. **The app never receives the real socket.** Only the proxy mounts it. The app's mount is
   the proxy's own socket, and `DBUS_SESSION_BUS_ADDRESS` points there. The socket directory is
   per app, so two apps with different grants cannot reach each other's bus.
3. **It fails closed.** An app that asked for a bus when no host bus can be resolved does not
   launch. Starting it without one would surface inside the app as an unexplained connection
   error, pointing nowhere near the config.

A filtered bus is also an agreement about uid: the proxy serves the socket as the invoking host
user, and an app in its own user namespace cannot connect. Validation therefore requires
`InternalUserMeta.KeepUserID`, rather than Zinc silently changing who the app runs as on the
strength of a bus grant. On a VM app `DBusMeta` is a validation error - a guest has no way to
take a bind-mounted unix socket.

### 5.8 Bus attribution (which app is a connection on the host bus)

A desktop watching the host session bus sees connections, not apps. The question it needs
answered is "which Zinc app and instance is this", and the answer has to come from something
the app cannot influence.

**Why not have each app own a name.** The obvious shape - give every instance a bus name like
`zinc.app.<app>.<instance>` - does not work, and it is worth writing down because it reads as
the natural design. `xdg-dbus-proxy` is a relay with no bus identity of its own: `--own=NAME`
grants THE APP permission to claim that name, it does not claim anything. The app then has to
volunteer to claim it, and a name a sandboxed app claims for itself is a self-assertion, which
is the exact thing attribution exists to stop trusting. A cooperative app would be labelled and
a lying one would be labelled as whatever it liked.

**What Zinc publishes instead** is the mapping it already holds by construction. Zinc creates
the proxy container, names it `zinc-dbus-<runtime name>` after an app it has already resolved,
and that container holds the only connection to the real bus the app is ever behind. Nothing in
that chain asks the app anything. Two commands expose it, both with a `--json` form, because a
desktop scripting against a documented constant becomes a second source of truth the moment
either side changes (same reason `zcr where` exists at all):

- **`zcr where <app[@instance]> [--json]`** - the layout of one instance: its state directory,
  its container name, and now its bus socket and proxy container. An app that asked for no bus
  reports `none` for both (JSON `"bus": null`), rather than a path that was never created.
- **`zcr bus [--json]`** - the attribution table: every running proxy, its host pid, and the
  `app@instance` it serves. This is the direction a desktop actually needs, since it starts
  from an observation and has no name to look up.

The chain a consumer walks, and how solid each link is:

1. **connection to pid** - `org.freedesktop.DBus.GetConnectionUnixProcessID` on the host bus.
   The bus took that pid from `SO_PEERCRED` when the connection was made: the kernel's answer
   about the peer, not the peer's claim about itself. Solid.
2. **pid to proxy container** - the pid column of `zcr bus`, read from the live runtime.
   Rootless podman does not remap pids, so the container's main process carries the same number
   the bus reported. Solid at the instant it is read, and **best-effort across time**: the bus
   captured its pid at connect time, so a proxy that has since died and had its pid reused would
   be attributed to whoever holds the number now. The window is small (the bus drops the
   connection when the proxy exits) and closing it entirely would need a bus that hands out
   something better than a pid.
3. **proxy container to `app@instance`** - the name Zinc gave the container, read back. Solid,
   with one recoverable ambiguity: an app name may contain dots and an instance may not, so
   `notes.local` reads either as the app `notes.local` or as `notes@local`. The store decides,
   and the only undecidable case is both existing at once, where the whole-name reading wins.

Two measured properties of what is actually observable, which matter before reading an empty
answer as "no app has a bus": `xdg-dbus-proxy` opens **one upstream connection per client**, so
an app with no live bus client contributes no host-bus connection at all, and an app with
several contributes several unique names - all carrying the proxy's one pid. Many-to-one is
normal.

`zcr bus` deliberately stops at the pid rather than resolving unique names itself. The consumer
is already holding a bus connection - that is how it saw the thing in the first place - and
putting a D-Bus client (a protocol implementation, an auth handshake, a dependency) inside the
sandbox runtime would buy no isolation.

---

### 5.9 Audio

`AudioMeta` grants sound one direction at a time. Each direction takes one of three forms,
and they are spelled differently because they are enforced differently:

| Form | Means | Enforced by |
|---|---|---|
| `none`, or absent | not granted | nothing to enforce |
| `default` | the session's own device, via the PipeWire socket | see below |
| `[/dev/snd/...]` | exactly these ALSA nodes | the kernel, through `--device` |

There are three capabilities, not two. `Playback` and `Microphone` are the obvious pair.
`Monitor` is the third: a PipeWire sink carries a `.monitor` source, a readable tap on
everything mixed into it, so a client on the session socket can record **what other apps are
playing**. That is what a screen recorder uses, and it crosses the boundary between two
sandboxed apps rather than between an app and a host device, since a music player and a video
call share a sink.

`Monitor` takes only `none` or `default`: a monitor source is part of PipeWire's graph and no
`/dev/snd` node carries one, so a device list there would look like a narrowing while doing
nothing, and is refused. It is also refused for a VM app, whose guest sees an emulated sound
card rather than the host graph.

Note what `Monitor` is not: it is a capability on the app doing the recording, never a
protection on the app being recorded. Zinc describes what an app may do, so there is no way
for a music player to declare its own output private - whether anything taps its sink is
decided by the other app's grant, in the other app's config.

**The device-list form is the strong one.** Those nodes are passed with `--device` and
nothing else on the sound subsystem is reachable, so an app granted one microphone cannot
open a second card. Naming a capture stream usually means naming two nodes, the card's
control node and its PCM: `/dev/snd/controlC0` and `/dev/snd/pcmC0D0c`.

**`default` is not yet enforcement on a container.** It mounts the session's PipeWire socket,
and PipeWire grants a client on that socket both directions plus the monitor sources that
record what other applications are playing. So `Playback: default` with `Microphone: none`
states something the runtime cannot currently hold the app to, and `zc` says so at authoring
time. Closing it means Zinc speaking PipeWire's security context (`pw_security_context_create`,
present since PipeWire 1.0) the way it already speaks Wayland's, creating a restricted
per-instance socket instead of handing over the session one. Until then, an app that must not
be able to listen should name its devices rather than take the default.

**On a VM, `default` IS enforced.** The guest is given an `hda-output` codec, which has no
capture stream, unless a microphone was granted, in which case it gets `hda-duplex`. There is
no capture endpoint for the guest to open. A device list is refused for a VM app, because a
guest cannot be handed a host character device.

Before schema v3 this was one flag, `Pipewire: true`, which mounted the socket and therefore
granted listening to every app that wanted to make a sound. The split exists so a config can
state the narrower thing, and so the gap between what it states and what is enforced is
visible rather than silent.

## 6. Networking model and startup ordering

This section expands 5.3: the scopes, the four tiers, the exact ruleset, and, at the end, how
dependent apps are started.

### 6.1 Scopes and direction

Each `NetworkList` entry is one directional rule. The fields that shape it:

- **`Host`** - scope. `false` (default) means the app's own netns. `true` means the host: a
  host-interface bind for LAN publishing (ingress), or host-scoped egress (not supported yet,
  6.5).
- **`AppName`** - for `Host: false`, which app's network. `""` (default) means this app
  itself; a name means a sibling app (a link, tier 2).
- **`Ingress`** - direction. `false` (default) is an **egress** rule: `Ports` are destination
  ports the app may reach. `true` is an **ingress** rule: `Ports` are the app's own listening
  ports, exposed to the scope.
- **`Blacklist`** - `false` (default) is a whitelist (default-drop; allow only what is
  listed). `true` is allow-all-except (default-accept with the listed entries dropped).
- **`IPv4CIDR` / `IPv6CIDR`** - destinations (egress) or allowed sources (ingress). The two
  families are emitted separately, so a v4 CIDR never gates v6 traffic.
- **`Domains`** - destinations named rather than numbered (egress only). Resolved AT LAUNCH
  into addresses on this list, under the same `Ports`. A snapshot, not name filtering, and not
  refreshed while the app runs - see 6.2.
- **`Ports`** - TCP+UDP port set for the rule. Not honoured on a `Via` list (the link is
  accepted whole), which is refused rather than accepted.
- **`Interface`** - a specific host/app interface to bind (pasta copies its addressing).
  Refused on an app that also has a sibling link: such an app is on bridges rather than pasta,
  where podman publishes by address and the interface could not be honoured.
- **`Via`** - on an egress list naming an `AppName`, send this list's CIDRs THROUGH that
  sibling instead of out this app's own egress (6.2). One list carries one address family.
- **`Forward`** - on a producer's own ingress list, agree to route for the siblings on its
  link. Never implied by another app naming it.
- **`ForwardPorts`** - what a forwarding app will carry, by destination port. Empty carries
  any port. The addresses are not repeated here: the client already fixed them by choosing
  what to route (6.2).
- **`GatewayV4` / `GatewayV6`** - next-hop for multi-homing (not supported yet, 6.5).

`NetworkMeta` also carries **`DNSServers`**: the resolvers the app is handed, and - once it
has any `NetworkLists` - the only ones its ruleset lets it reach. Required for a routed app,
whose first resolver must be IPv4 (6.2).

List order is priority: the first entry wins. Blocking DNS, for example, is just an egress
blacklist for ports 53 and 853, ordered ahead of any broad allow so it wins. Validation
rejects an **egress port rule with no destination CIDRs** (it would silently no-op and leave
those ports open); name `0.0.0.0/0` and/or `::/0` for "everywhere", or drop the ports. An
ingress list needs no CIDR - empty means "any source".

### 6.2 The four tiers

| Tier | Config shape | Result |
|------|--------------|--------|
| Isolated | no `NetworkLists` | `--network none`; the app reaches only its own localhost |
| Egress   | egress list(s), self-scoped | default-drop; allow only the listed destination CIDRs and ports |
| LAN publish (tier 3) | `Ingress: true` + `Host: true` | publish the app's own ports to the LAN via pod `-p` forwards, filtered by source address |
| Sibling link (tier 2) | egress list naming another app's `AppName` | a private `--internal` bridge between the two apps, gated per-port |

**Sibling link, in detail.** A consumer's egress list that names a producer's `AppNameID`,
and the producer's own self-scoped ingress list, attach both apps to a private, internal
bridge `zinc-link-<producer>` with fixed interface names (`zlink0`, `zlink1`, ...) and a
network alias equal to each app's `AppNameID` (so a consumer connects to `<producer>:<port>`).
The producer accepts only its published `Ports` inbound on that link interface; everything
else default-drops. The consumer accepts nothing new inbound. The bridge is `--internal`, so
neither app reaches anything else through it.

**Allowing a destination by name.** An egress list may carry `Domains` alongside its CIDRs.
Each name is resolved **at launch** and its addresses join that list's allowed set, under the
same `Ports` - so the renderer only ever sees addresses and the ruleset is ordinary nft.

Read the guarantee precisely, because it is narrower than "this app may only talk to these
domains":

- What is enforced is at the IP layer, on the addresses those names held when the app
  started. An app that resolves somewhere else and connects is dropped. An app that connects
  to one of those addresses **by number**, without asking DNS, is allowed. Nothing inspects a
  hostname on the wire, and nothing here would notice one.
- Addresses shared by other names are shared by this rule. A domain on shared hosting or
  behind a large CDN allows every other name on the same address.
- **The snapshot is not refreshed while the app runs.** A domain whose addresses rotate will
  drift out of the set and the app loses access to it until it is restarted. That direction
  is deliberate: a stale entry stops working rather than quietly allowing whoever holds the
  address now. Making it live needs a resident process in the netns updating an nft set from
  DNS answers, which is a different design and is not what this is.
- A name that does not resolve **fails the launch**, naming the domain. An app that starts,
  looks healthy, and cannot reach the one host it exists to talk to - with the reason visible
  nowhere - is the worse outcome.

Refused rather than accepted-and-mis-enforced: `Domains` on an ingress list (an incoming
packet carries an address, not a name, so resolving would admit whoever holds that address),
on a blacklist (blocking a name would mean blocking every address it is *not* resolved to,
and the rule would read as a ban while stopping only today's addresses), and on a sibling
link (gated by interface, so an address set on it enforces nothing).

**DNS for a routed app.** `NetworkMeta.DNSServers`, required once a list sets `Via`. podman
writes a container's `resolv.conf` and points it at the network's own resolver; on an
`--internal` bridge that resolver answers sibling names and forwards nothing, so a routed app
gets NXDOMAIN for anything external. Zinc therefore does two things: it hands the pod the
declared resolvers (`--dns`), and - because the app may still hold a resolver of its own, and
because what matters is where the query GOES rather than what `resolv.conf` says - it
redirects the query in the app's netns: DNS to any
address is rewritten (`nat` `output`, `dstnat` priority, so the filter hook then sees the new
address) to the declared resolver, which is routed through the sibling like anything else - it
travels inside the tunnel and stops with it. Whatever is not redirected is dropped. Only a
routed app is redirected: for an ordinary one the network's resolver works and is the only
thing that knows its siblings' names.

**Routing through a sibling.** An egress list that names an `AppName` and sets `Via: true`
sends its CIDRs to that sibling over their private link instead of out this app's own egress -
how an app is put behind a VPN container. It is per list, so one app can pick a different
backend per destination. The client gets no other path to those destinations, so it cannot
leak past the sibling, and when the sibling stops its traffic blackholes rather than falling
back (measured, both ways).

The sibling must agree: `Forward: true` on its own link ingress list. Forwarding for other
apps makes an app a router, so it is never implied by another app naming it. A forwarding app
gets `net.ipv4.ip_forward=1` at pod creation - a container cannot set it itself, `/proc/sys`
is read-only in the namespace - plus a default-drop `forward` chain and `masquerade`, without
which replies would be addressed to a private link address the outside cannot route back to.

**What a gateway carries is bounded from both ends, and the two ends belong to different
apps.** *Where* is the client's: only the CIDRs its own `Via` list names are routed to the
gateway at all, and the client cannot change that - the runner installs those routes and the
app has no capability to alter them. *What* is the gateway's, and it is `ForwardPorts`. A
gateway that sets `ForwardPorts: [53]` is a DNS hop: whatever a client points at it, only
port 53 crosses. Empty carries any port, which is what a general-purpose gateway is for.

A gateway's **own** egress rules deliberately do not bound what it forwards. They say where
*this app* may go; forwarded traffic is somebody else's, and was already bounded by whoever
sent it. The `forward` hook is a separate chain from `output`, so the two never see each
other's rules - that is a design line, not an oversight.

`zc new <name> --image <img> --tunnel <wg.conf>` authors one in a single command, and authors
it *working*: it reads the config and adds the egress rule the handshake needs, allowing UDP
to each peer's endpoint. Setting the path alone would produce an app that cannot work and
does not say why - the tunnel is built inside a namespace whose ruleset default-drops, so
without that rule the handshake never leaves and the interface sits there carrying nothing.
The endpoint is already in the file, so there is no reason to copy it across by hand and no
reason for the two to be able to disagree.

**Zinc builds the tunnel, so the app never holds the capability that builds it.**
`NetworkMeta.Tunnel.WireGuardConf` points at a wg-quick-format config; the runner reads it at
launch, creates `wg0` inside the app's netns, applies the config, assigns the addresses and
routes the peers' `AllowedIPs` into it - all in the same privileged helper that already
installs routes and loads the ruleset, and all before the app exists.

That split is the whole reason this exists. A tunnel needs `CAP_NET_ADMIN` to create, and an
app with `NetworkLists` may never hold it: `NET_ADMIN` in the pod netns would let the app
flush the ruleset that contains it, so validation refuses the pairing. Which meant a gateway
could be a NAT hop and never an actual tunnel endpoint. Now the capability lives in a
container that has exited by the time the app starts, and the app inherits a working tunnel
with an empty capability set (measured: `CapEff: 0000000000000000`, a completed handshake,
and traffic crossing).

Three details that are not incidental:

- **The private key travels on the helper's stdin**, the same channel the nft ruleset uses.
  Not in an argv - every process on the host can read that out of `/proc` - not in an image,
  and not in a mount the app could open.
- **The endpoint keeps its pre-tunnel route.** Before the peers' `AllowedIPs` are routed into
  `wg0`, each peer endpoint is pinned to whatever the namespace already routed by. Without
  that, a `AllowedIPs = 0.0.0.0/0` config would send the encrypted packets carrying the
  tunnel into the tunnel. It is also why an `Endpoint` must be an address and not a name: the
  pin happens before the namespace closes, and a name could resolve elsewhere by then.
- **wg-quick's script directives are refused, not ignored** - `PostUp`, `PreUp`, `PostDown`,
  `PreDown`, `SaveConfig`, `Table`. The first four are arbitrary shell and the helper that
  would run them holds `NET_ADMIN` in the app's namespace; a config file is not a place to
  accept code from. `DNS` is refused too: `NetworkMeta.DNSServers` is the one place to say
  that, and two would disagree.

**Gateways chain.** The forward chain accepts out of every interface the gateway's own routes
can use - its egress bridge, and any link it is itself routed through - with `masquerade`
following onto each. So a hop can pass its clients' traffic onward into another gateway
rather than out to the network, and a relay whose every list is a link has no egress bridge
at all.

The gateway's address is never written into a config. podman assigns it and it changes when
the gateway is recreated, so the route step resolves it at launch through the network alias
podman already gives every app on a link. It runs before the ruleset, because resolving needs
DNS and the ruleset closes the netns - and both run before the app, so the app still never
sees an unlocked network.

A gateway is worth a `StartConditions.ReadyCheck` (6.6): a routed client whose gateway is up
but whose tunnel is not has nowhere to send anything, and the readiness gate is what makes
`DependsOn` wait for the tunnel rather than for the container.

**A linked app may also reach the outside.** One app is gated both ways at once: the `zlink*`
bridges by interface, everything else by address and port. Chain policy comes from the
non-link lists alone - a link list is structurally a whitelist, so counting it would flip an
app that pairs a link with an all-blacklist egress to default-drop and silently deny what the
blacklist meant to leave open. Such an app cannot use pasta: podman refuses more than one
network unless it is in bridge mode, so it gets `zinc-egress-<app>` instead, a routable bridge
of **its own**. Not the shared default bridge - apps on one bridge can reach each other over
L2, which would leave isolation resting on the nft rules, and an app whose egress list is an
all-blacklist runs default-accept. A link-only app is unchanged and stays on its private
bridges alone.

### 6.3 The nftables ruleset

The ruleset is a pure function of the validated config, rendered as `table inet zinc`. A
standard (egress and/or tier-3) app builds an `output` chain (egress: `daddr`/`dport`) and,
when it publishes, an `input` chain (ingress: `saddr`/`dport`). A tier-2 (sibling-link) app
is gated by interface as well: the private `zlink*` bridges are accepted, a producer's
published ports are accepted inbound on its own link interface, and any address rules the same
app carries are still emitted alongside. (Both kinds are rendered at once; before 0.7 it was
one or the other, and whichever ran ignored the other kind of list outright.) An app with any
link always gets an input chain, published or not - it shares that bridge with its siblings,
and a hook with no base chain is not filtered at all.

Every chain defaults to `policy drop` and always accepts loopback and
`established,related` traffic (so a server's reply rides the established rule). Per-direction
policy follows that direction's lists: a whitelist present means default-drop (fail-closed);
an all-blacklist direction means default-accept with the listed drops as high-priority
carve-outs. A direction with no lists stays default-drop - a pure publisher gets no egress; an
egress-only app gets no input chain at all.

A dry run (`zcr run <app>` with no `--exec`) prints the exact `podman` commands **and the nft
ruleset that would be piped in**, so what will be enforced is fully visible before anything
runs.

**Counters, and which rules get one.** Rules that decide something carry a `counter` and a
`comment` naming them; `zcr net <app>` (5.3) reads them back with `nft -j` and reports each by
its comment, so nothing has to reconstruct nft syntax out of JSON. What is counted is a
deliberate subset, because a counter on every rule buries the two numbers worth reading:

| Counted | Left bare |
|---|---|
| every rule a `NetworkList` produced, labelled `list[N]` by its index in the config | `oif "lo"` / `iif "lo"` - the app talking to itself |
| the link and tunnel accepts (`link zlink0`, `tunnel wg0`) | `ct state established,related` - every packet of every flow already allowed |
| the DNS deny (`undeclared dns udp`/`tcp`) | the DNS accepts to the resolvers the app declared |
| the default-drop backstop (`default policy`) | the `forward` chain's accepts, and the `nat` rules |

Two of those choices are load-bearing. The **backstop** is a trailing `counter drop` written
into every default-drop chain, because nftables counts rules and not policies: a fail-closed
chain refuses by policy, so without it the most valuable number - what the sandbox is actually
refusing - would be permanently zero whether the lock-down worked or not. It changes no
behaviour (a packet reaching the end of the chain was dropped anyway) and is emitted **only**
for a drop policy: on an all-blacklist chain the same line would turn allow-all-except into
deny-all. And the **conntrack accept stays bare and stays above them**, which is what gives an
accept counter its meaning: it counts the packets that opened flows, not the traffic those
flows carried. That is the more useful reading - "was this rule exercised, how often" - and it
is why the byte column is small. A `list[N]` label points at the entry's index in
`NetworkMeta.NetworkLists`, not its position among the egress rules, so it names the line of
config that produced it even when a link list sits ahead of it.

### 6.4 The netfilter helper image

The nft step runs inside a tiny helper image, `zinc/netfilter:local`, built once with `make
netfilter-image` (in `container/runner`). It is a digest-pinned Alpine base with a
version-pinned `nftables`. It is referenced by a local tag and run with `--pull never`, so the
privileged step is always the locally vetted build and never a registry pull; a missing image
fails fast with a clear error. It runs with `--cap-drop all --cap-add NET_ADMIN
--security-opt no-new-privileges`, reads the ruleset on stdin, and exits - the rules persist
in the pod's netns for the app that starts next.

### 6.5 Not supported yet (rejected, not run)

The runtime is fail-closed: a config it cannot enforce correctly is **rejected at launch**,
never silently mis-enforced. Rejected in this build:

- **Host-scoped egress** (`Host: true` with an egress list).
- **Gateway / multi-homing** (`GatewayV4` or `GatewayV6` set) - the fields are schema-legal,
  but a config using them is rejected.
- **An ingress list that targets an `AppName`** - contradictory (a producer publishes to any
  sibling that joins its link; the consumer names the producer).

`Configs` are mounted from the app's own bundle at
`$XDG_CONFIG_HOME/zinc/apps/<app>/configs/<BundlePath>`, read-only unless `Writable` says
otherwise. Per app rather than per instance: a config file is content the app was authored
with, so every instance reads the same one, and per-instance content is runtime state under
the state directory instead.

**Notifications.** `NotificationMeta` is enforced by a filter that stands between the app and
its D-Bus proxy, so the chain becomes app -> filter -> `xdg-dbus-proxy` -> session bus. It
exists because the proxy filters by NAME: whether an app may reach
`org.freedesktop.Notifications` is a question `xdg-dbus-proxy` answers, and what an app may put
IN a notification is a question about a message body.

The filter reads only `Notify` calls and forwards everything else as the bytes it arrived as,
file descriptors included - the same connection carries the app's portal traffic, and a relay
that re-encoded or dropped that would break file dialogs and screen sharing. `Disabled` is
answered with `AccessDenied`, so the app is told; `Silenced` is answered with a plausible
notification id, so the app cannot tell its notification went nowhere. `UseCustomPrefix`
rewrites the summary, `AllowedActions` drops the action list, `AllowedLinks` strips anchor
markup from the body, and `AllowedProlonged` clamps an expire timeout to ten seconds - a
notification that never expires owns a corner of the screen, which is a thing to grant.

A `Notify` call is decoded and re-encoded rather than patched, because D-Bus alignment is
measured from the start of the message: changing the length of the summary moves the padding
inside the hints dictionary that follows it. A zero `NotificationMeta` keeps the filter out of
the launch entirely, and validation refuses the block on an app whose `DBusMeta` cannot reach
the notification service, since a policy over traffic that cannot happen is not a policy.

A guest's network is section 10.5: the model is the same, but what carries it is
qemu and pasta rather than a pod.

### 6.6 Dependency startup ordering

`StartConditions.DependsOn` lists apps that must be up before this one. On launch the runner
brings them up first, depth-first, so a dependency's own dependencies come up before it. An
already-running dependency is left untouched; a dependency cycle is reported as an error
rather than recursed into forever. This is why launch is a single orchestrated path (the app
layer, section 13) rather than a bare `podman run`.

**Running is not ready.** By default a dependency counts as up once its container is, which is
true enough for a service whose process is its readiness and false for anything a dependent
routes through: a VPN container is running long before its tunnel is, and a client started in
that window has a default route and a resolver pointing at a gateway that cannot forward yet
(6.2, routing). `StartConditions.ReadyCheck` closes that window. It is a command in exec form
(`["sh", "-c", "ip link show wg0 | grep -q UP"]`) which the runner quotes word by word and
installs as the container's healthcheck, so `podman ps` reports health for the same question
the launch sequence waits on, and which a dependent polls until it passes or `ReadyTimeoutSec`
(default 60s) runs out. It is installed in the `CMD-SHELL` form rather than the tidier JSON
exec form, which podman 5 understands and podman 4.9 - what Ubuntu LTS ships - does not; a
launch-blocking gate has to work on the podman people actually have, so the image needs a
shell.

A dependency that never becomes ready fails the dependent's launch rather than letting it
start anyway: for the routed case, starting is not degraded operation, it is an app whose
every connection fails for a reason nothing reported. Only a dependency this launch started is
waited on - one that was already running was gated the same way by whoever started it.

---

## 7. Images: derived builds and base images

**Third-party images are pulled and pinned by digest** (5.5). **Derived images** are the
quick-setup path: instead of authoring a Containerfile, an app sets `ImageMeta.Install` to one
or more shell lines, and Zinc builds a small image `FROM <ImageMeta.Image>` plus a single
`RUN` layer carrying those lines (joined with `&&`, so a multi-step setup fails fast). The
app then runs that derived image instead of the bare base.

- The derived image is tagged locally, `zinc/app-<name>:local`, built with `-t`, run with
  `--pull never`, and never pushed.
- Its `FROM` base is `ImageMeta.Image`, which 5.5 forces to be digest-pinned (or a
  `localhost/` ref), so the build starts from a known base. Because a per-machine local build
  has no meaningful registry digest, the derived image is not itself digest-pinned; its
  guarantee is the pinned base plus the visible install lines.
- Freshness is tracked by an OCI label, `zinc.build`, holding a fingerprint of the inputs
  (the base image plus the install script). A plain `zcr run` rebuilds automatically when the
  image is missing or the fingerprint differs, so an unchanged app reuses its image and a
  re-pinned base or edited install takes effect on the next run. `zcr build <app>` (or `zc
  build`, or the TUI build action) forces a rebuild.
- The install line runs through the image's own `/bin/sh`, so a distro package-manager
  invocation works exactly as typed. The form offers a per-distro hint (apt for
  debian/ubuntu, apk for alpine, dnf for fedora/rhel, pacman for arch, zypper for openSUSE),
  derived from the base image name; it is UI sugar only and never constrains what may go into
  `Install`.

The example `hollywood.yaml` app is a derived-image demo: a stock digest-pinned Debian base
plus `apt-get install -y hollywood`, run in a terminal window.

Locally built base images (for example a language toolchain image to layer projects on) are
referenced by a `localhost/` tag and are exempt from the digest pin; trust comes from the
vetted, version-pinned recipe rather than a global digest, and their own `FROM` base is still
digest-pinned. Shipping a curated set of such base images is on the roadmap.

---

## 8. Build and Release

**Podman-only. There is no host Go for tool builds.** Every Go command - `test`, `vet`,
`fmt`, `vendor`, `build` - runs inside a **digest-pinned `golang` container**, invoked
through `make`. There is no `go run`: `make build` produces a static binary in the container
and you run that binary.

The build logic is shared, not copied. A single repo-root `Containerfile` (the pinned Go
toolchain; it builds whichever module is the build context), a `check.mk` of containerized
checks that every module includes, and a `tool.mk` of binary targets that each tool's
three-line `Makefile` includes. "The same logic, only different paths."

From any module (`common`, `container/runner`, `creator`):

```sh
make check      # gofmt + go vet + go test, in the pinned container
make build      # reproducible build -> ./bin/<tool>
make repro      # build twice, assert the binary is byte-identical
make vendor     # refresh vendored deps (the only step that needs network; GOWORK=off)
```

Reproducibility is enforced by pinning every input: the Go toolchain by digest
(`GOTOOLCHAIN=local`, so it never silently downloads a different one), dependencies by each
module's `./vendor` plus `go.sum` (built with `-mod=vendor`, no proxy, no network), and the
build flags themselves (`CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, `-ldflags="-s -w
-buildid="`). Same inputs, same bytes, on any machine.

**Versioning.** Each binary carries a `version`, stamped from `git describe` via
`-ldflags "-X main.version=..."`; `zc version` and `zcr version` print it. A plain build with
no git falls back to `dev`.

**CI** (`.github/workflows/ci.yml`) runs three jobs: `common` (gofmt / vet / test on the
schema and validation), `check` (a matrix of `container/runner` and `creator`, each
running `make check` and `make build`), and `e2e` (the end-to-end suite driving the real
binaries against podman).

Every module in the tree is built and checked in CI. There is no separate VM creator: `zc`
authors both app types, so the old `virtualization/creator/` skeleton was deleted in 0.4
rather than migrated.

---

## 9. Components

### 9.1 zc - the creator

**Stack:** Go + Bubbletea. `zc` authors app files and manages them; it depends only on the
shared `common` library and never imports the runtime.

Authoring commands (local, no runtime needed):

```
zc new <name> --image <img> [--desc d] [--icon i] [--tunnel wg.conf]
zc list
zc validate <name|app.yaml> [--resolved]
zc delete <name>
zc keys list|show|set <s>|edit|validate|path
zc compose export <name> [-o f]     zc compose import <compose.yaml> [--service s] [--dry-run]
zc tui
```

Runtime commands (forwarded verbatim to `zcr`, see 9.2):

```
zc run <name|app.yaml> [--exec]     zc build <name|app.yaml>
zc stop|restart|inspect <name>      zc logs <name> [-f]
zc term <name> [--shell]            zc image search <term>|resolve <ref>
```

A bare `<name>` resolves against the store (`~/.config/zinc/apps`); an argument that looks
like a path (contains `/` or ends in `.yaml`) is read directly.

**The TUI** is the keyboard-first manager: create, edit, run, stop, logs, delete, rename. Its
form footers show only the gestures that actually apply to the focused field and the app's
current state, drawn from the active scheme, so the hints stay honest instead of a fixed wall
of keys. An **advanced** row opens the full YAML in `$EDITOR`, where capabilities, network
lists, volumes, and keys are edited directly. The TUI's run/stop/build/term/logs actions all
go through `zcr` (via the backend facade); `zc` never runs a container itself.

**Keybind schemes.** `zc`'s own TUI keys are not hardcoded: they resolve through a
selectable scheme. Two are built in (`default` and `vim`) and users can define their own under
`~/.config/zinc/zc`. `zc keys list|show|set|edit|validate|path` and an in-TUI picker (open
with `?`) choose and apply one, switching live. These are `zc`'s interface keys only, an
implementation detail of the creator; they are distinct from any desktop hotkeys, which are a
host-level (ZDE) concern. (The scheme files happen to be TOML internally; that is a `zc`
implementation detail and has nothing to do with the app format, which is YAML.)

**Compose interop** (`internal/compose`) translates between an app definition and a
Compose-specification file, in both directions. Both are authoring, which is why they live
here and not in the runner, and neither is lossless - so both print what did not cross rather
than omitting it silently.

*Exporting drops guarantees.* A compose file cannot express the nftables egress lock-down
applied to the app's netns before it starts, the Wayland security context, or the desktop
wiring the runner resolves from the live session. What it can carry - image, entrypoint, user,
capabilities, resource limits, mounts, published ports, `depends_on`, and the `ReadyCheck` as a
`healthcheck` - it carries, and the generated file leads with a comment saying it describes an
app rather than sandboxing one. A VM app is refused outright: a guest is not a container.

*Importing tightens.* Compose has no way to say what a service may reach, so reading its
silence as "full network access" would import a posture nobody chose. An imported app arrives
with no NetworkLists, which is no network at all; published ports are the one exception,
because they are stated. Everything else fails closed the same way: an unqualified mount
becomes read-only and noexec (compose's default is read-write), `cap_add: ALL` is refused, a
bare numeric `user` is dropped because Zinc passes the user to podman by name, and a mutable
image tag is resolved to its digest through `zcr image resolve` before the app is saved -
without which validation would reject it anyway. Each service becomes its own app, and
`depends_on` becomes `DependsOn` (with the caveat, printed, that ordering is not connectivity:
in compose the two share a network, in Zinc a link has to be declared).

Internally `zc` is a small CLI over a backend facade: an `internal/store` YAML app store (a
mirror of the same on-disk format `zcr` reads), an `internal/runner` delegate that finds `zcr`
on `$PATH` and drives it, an `internal/backend` facade the CLI and TUI both call, the
`internal/tui` Bubbletea UI, `internal/compose` for the interop above, and `internal/keys` for
the keybind schemes.

### 9.2 zcr - the runner

**Stack:** Go. `zcr` is the runtime. It reads an app file and runs it as a rootless podman
container, applying the network lock-down before the app starts (5.3). `zc` shells out to it,
but it is a first-class CLI you can drive directly:

```
zcr run <app> [--exec]      print the launch plan, or launch it (--exec)
zcr build <app>             (re)build the derived image (ImageMeta.Install)
zcr validate <app>          parse + validate; report problems and warnings
zcr stop|restart|inspect <app>
zcr logs <app> [-f]         zcr term <app> [--shell]      zcr ps
zcr where <app[@instance]> [--json]
                            state dir, container name, bus socket and bus proxy
zcr bus [--json]            the bus attribution table: proxy, host pid, app@instance (5.8)
zcr net [app] [--json]      network posture of every running app, or one app's counters (5.3)
zcr image search <term> | resolve <ref>
```

Without `--exec`, `run` is a dry run: it validates and prints the exact `podman` command(s)
and any nft ruleset that would be enforced. This is the "no magic" principle in action.

The launch path is a single orchestrated sequence (validate -> gate unsupported network
shapes -> auto-start dependencies -> build the derived image if needed -> run the network
lock-down through the `NetEnforcer` -> start the app container detached), so there is exactly
one path to get right.

**Terminal apps.** A GUI app renders through the passed-in Wayland socket. A CLI/TUI app
(`StartConditions.Terminal`) is launched inside the host's terminal emulator with an
interactive TTY - a container otherwise has no terminal to attach to. The emulator argv comes
from `ZINC_TERMINAL` (else `$TERMINAL`), so both `foot` and `xterm -e` forms work; launching a
terminal app with neither set fails with a clear message. See section 11.

**Multiterminal apps.** A terminal app may also set `StartConditions.Multiterminal` to attach
many terminals to one shared instance. The container runs a detached **holder** (`sleep
infinity` under `--init`, so `podman stop` is prompt), and every terminal is a `podman exec
-it` into it, running the app's own command (or a shell with `--shell`). The app lives until
the **last terminal closes**, unless `StopConditions.Background` keeps the holder running.
Coordination is by filesystem flock under `$XDG_RUNTIME_DIR/zinc/run/<app>/`, with no daemon
and no socket: a per-app lock serializes holder start-up, each terminal's waiter flock-holds a
liveness marker for its life (auto-released on death, so a killed terminal cannot wedge the
count), and the last waiter out stops the container. Because a holder owns PID 1, a
multiterminal app needs an explicit entrypoint (the image default cannot be replayed into each
terminal), which validation enforces.

### 9.3 zlt - the launcher (TUI)

**Stack:** Go + Bubbletea. `zlt` (zinc-launcher-tui) is a fast, keyboard-first fuzzy
picker over the defined apps. It lists `~/.config/zinc/apps`, filters as you type (a small
in-house subsequence matcher that favours matches at the start of a name and at word
boundaries), and on **enter launches the selected app by shelling out to `zcr run <app>
--exec`**, then quits (dmenu-style). So `zcr` still does the real work - validation,
dependency auto-start, the derived-image build, the network lock-down - and `zlt`, like
`zc`, depends only on `common` and never imports the runtime. A `zlt <app>` form launches
one app directly (for a desktop hotkey or a script), and a `●` marks apps already running
(best-effort, from `zcr ps`). It lives at `launcher/tui`; the read / launch / match logic it
shares with `zlg` lives in the `launcher/common` library.

### 9.4 zlg - the launcher (GUI)

**Stack:** Go, rendering in pure Go with no cgo. `zlg` (zinc-launcher-gui) is the graphical
sibling of `zlt`: the same quick picker over the defined apps, for a point-and-click or
keyboard launch. The picker window itself is the reusable **`menu` module** (below); `zlg` is
a thin consumer of it - it loads the defined apps, marks the ones `zcr` reports running, and
hands `menu.Run` an activate closure that launches the chosen app through `zcr`. So, like
`zlt`, it takes the read / launch / match logic from `launcher/common`, shells out to `zcr`,
and never imports the runtime. `zlg <app>` launches one app directly.

Because it builds on `menu`, `zlg` is a **static, `CGO_ENABLED=0`, runs-anywhere,
byte-reproducible** binary built from the same minimal image as the other tools (no graphics
libraries, no dynamic linking), and it is a real **`wlr-layer-shell` floating overlay** - a
centered, keyboard-grabbing panel that floats above the tiled windows, the way fuzzel/wofi
do, not a tiled window. Known limit (0.3): the keymap is US-QWERTY (full xkb layout support
is future work).

**The `menu` module** (repo-root `menu/`, module path `github.com/crispuscrew/zinc/menu`) is
the overlay-menu core, extracted out of `zlg` so any program can build its own menus over it.
It speaks the Wayland wire protocol directly (go-wayland) plus a **hand-written
`wlr-layer-shell` binding** (`menu/layershell.go`, since go-wayland ships only core +
xdg-shell), software-renders a fuzzy-filtered list into a shared-memory buffer with a bundled
bitmap font, and reads the system light/dark palette from the XDG desktop portal over D-Bus.
Everything but the Wayland event loop is a pure, unit-tested package (`internal/picker`,
`internal/keymap`, `internal/render`, `internal/theme`, and the `internal/match` fuzzy
matcher). Its whole public API is one call, `menu.Run(items, activate, opts)`. It
deliberately depends on **no** Zinc sibling module (Go `replace` directives are not
transitive, so a sibling dependency would make it un-importable from another repo), so `zde`
and a future wofi-like picker can import it too.

### 9.5 zvr - the VM runner

`zvr` (section 10) is the virtualization sibling of `zcr`, sharing the same `common` schema
library so every tool uses one config format. It depends only on `common` and drives
`qemu-system-x86_64` directly.

---

## 10. Virtualization (zvr)

For isolation beyond what containers provide - untrusted GUI apps, foreign OSes, throwaway
environments - Zinc runs VM apps alongside container apps. Containers remain the primary
runtime; VMs are the heavy-isolation escape hatch. Both kinds are authored by `zc` into the
same store and split by `Type`, and each runner refuses the other's apps by name rather than
half-running them.

### 10.1 Why qemu directly, and not libvirt

Earlier drafts of this document specified libvirt. The implementation does not use it, and
the reason is worth recording because it is not a matter of taste.

libvirtd spawns the qemu process, so that process is not in the user's session and cannot
open a window on their compositor. libvirt's answer is SPICE plus a separate viewer, which
adds a hop between the guest's frames and the screen. For a VM that exists to run something
interactive - a game, anything real-time - that hop is the whole problem.

Driving qemu directly means `zvr` starts it as a child of the user's session, so the guest
gets a local, GPU-accelerated window whose frames never leave the machine. It also matches
the container side exactly: build an argv from validated config, exec a binary, print the
command with `--dry-run`.

The cost is real and is paid deliberately: **`zvr` owns supervision.** Starting, finding and
stopping guests is its job, where libvirt would have provided it, along with snapshots and
managed save, which `zvr` does not have.

### 10.2 How a guest runs

A launch is: validate the config, verify the base image against its pinned digest, create
the app's overlay if it has none, rebuild its cloud-init seed, compose the argv, wrap it in a
filtered network namespace if the app declares one (10.5), start the process. Nothing is
created for a config that does not validate, and no guest starts from a base image that no
longer matches its digest.

**Disks are copy-on-write.** The base image named by `ImageMeta.Image` is never opened for
writing; each app gets its own qcow2 overlay backed by it, so `zvr reset` deletes the overlay
and the app is back to its authored image. That is the VM reading of a container's
disposability.

**The base is pinned by `VirtualizationMeta.BaseDigest`**, the sha256 of the file's bytes. A
container digest rides inside its reference; a file's cannot, so the pin is its own field -
but the rule is the one from section 5.5: what runs is what was authorised. The image is
hashed in full on first use and then whenever its identity moves (device, inode, size, and
both timestamps at nanosecond resolution). That catches a base that was replaced, rebuilt or
restored. It is not a defence against someone who can already write to the image directory,
because they can rewrite the cache alongside it.

**Supervision is a pidfile plus QMP.** The same control socket carries the ACPI power button,
so a graceful stop lets the guest's own OS flush and unmount rather than being killed
mid-write; SIGTERM and then SIGKILL stand behind it. A pidfile is checked against `/proc`
before anything is signalled, because pids are recycled.

The pid in that file is written by `zvr`, not by qemu, and for a filtered guest the difference
is the whole thing: pasta's namespace is a PID namespace too, so qemu is pid 1 inside it and
`-pidfile` records `1` - which on the host is init. The usable pid is found from the host side
and stored instead. Signalling what qemu wrote meant signalling nothing, and every stop left a
pasta and a qemu behind.

**The guest's hardware is exactly what the config asked for.** qemu is started with
`-nodefaults`, so nothing arrives merely because it was compiled in, and the host process is
confined by qemu's own seccomp jail (`-sandbox on`, denying privilege elevation, helper
spawning and scheduling changes).

### 10.3 Display and the boundaries that do not carry over

`VirtualizationMeta.Display` is explicit, never inferred: `Accelerated` attaches
`virtio-gpu-gl` and a local window, so guest 3D runs on the host GPU and reaches the
compositor as a dmabuf; `Window` is the same without acceleration; `None` is headless with a
serial console; `Compatible` is an unaccelerated framebuffer for a guest that has no
virtio-gpu driver at all. Accelerated 3D needs a guest with that driver, which in practice
means Linux.

Two container mechanisms deliberately do **not** carry over, and are rejected rather than
approximated:

- **The egress lock-down.** It is nftables applied inside a container's own network
  namespace; a guest has its own kernel and none of that reaches it. A VM app uses user-mode
  networking with explicit `ForwardPorts`, each bound to 127.0.0.1 so a guest service reaches
  the host that started it and not the LAN. `NetworkMeta` on a VM app is a validation error.
- **Host mounts, keys, capabilities and the theme bundle.** All of these are bind mounts or
  kernel features of a shared kernel. Sharing a directory into a guest needs virtiofs, which
  this build does not implement, so those fields are refused on a VM app with a message
  saying what supporting them would take.

A field that looks configured while doing nothing is worse than one that refuses to save,
because the author believes in a boundary that is not there. This is the same principle as
the network model rejecting what it cannot enforce.

### 10.4 A Windows-class guest

Windows is not Linux with a flag flipped. It needs a different machine, and the fields that
describe it are separate and explicit rather than a "Windows" preset, so the config says what
the machine has and the guest either drives it or does not.

**Devices the guest has drivers for.** `Devices: Compatible` gives an AHCI disk, an Intel
e1000e NIC and a USB tablet. Windows Setup ships drivers for none of the virtio hardware, and
pointed at a virtio disk it reports finding no drives at all, so this is a property of the
machine rather than a performance preference.

**Firmware, and a failure that is nearly invisible.** Windows 11 requires UEFI, Secure Boot
and a TPM 2.0. Secure Boot needs SMM: the firmware keeps its signature database in memory only
System Management Mode may write, and without it OVMF runs but does not enforce anything, so
the guest reports Secure Boot switched off. The TPM is swtpm over its **control** socket, not
its data socket; pointed at the wrong one qemu blocks forever before it opens a window.

The sharpest edge is which OVMF build is used. Distributions ship two generations side by
side, and only the current 4 MB build carries the TPM driver. QEMU publishes the TPM's ACPI
device itself, so on the legacy 2 MB build a guest still enumerates it and still binds a
driver to it. Only Windows notices there is nothing behind it, and all it says is that the PC
does not meet its requirements. Zinc prefers builds that hand the TPM over, warns when only a
legacy one exists, and refuses a variable store written by the other generation rather than
letting qemu boot a quietly wrong Secure Boot state.

**A machine identity of its own.** Left alone, every qemu guest reports the SMBIOS UUID
`00000000-0000-0000-0000-000000000000` and the MAC `52:54:00:12:34:56`. Windows Autopilot
identifies a device by a hash over those fields, so a guest with the defaults can match a
stranger's corporate enrolment and reach OOBE demanding a sign-in to their tenant. Both are
derived from the app name: unique per app, and stable, because Windows treats a changed UUID
as swapped hardware and asks to be reactivated. `MacAddress` overrides the NIC's address for a
guest that should not announce itself as a QEMU machine at all.

**A screen size fixed at boot.** A guest with no display driver takes the mode the firmware
gave it and cannot be told about another, so the resolution is chosen once and resizing the
window only scales those pixels. Plain VGA cannot be given one, which is why an unconfigured
guest is always 1280x800. `DisplayWidth`/`DisplayHeight` switch it to a display the firmware
does take a size from, along with a framebuffer sized to the screen and an EDID refresh rate
low enough that the mode's pixel clock fits its own field. Every one of those limits fails the
same silent way, by coming up at 1280x800 with nothing logged, so a size that cannot work is
refused at validation instead. What validation cannot check is the guest's own ceiling: an
installed Windows desktop with no display driver paints at most 1824x1080 and, asked for more,
writes rows of its own width into a wider scanout - a sheared picture rather than a smaller
one. That limit belongs to the guest, not the machine, so it is documented rather than
enforced; a Linux guest on the same device drives 4K perfectly well.

**A provisioning disc in the guest's own language.** Every VM app gets a small read-only disc
built fresh on each launch. For a cloud-init guest it carries that guest's identity. A guest on
the compatible device profile has never heard of cloud-init, so the disc carries
`zinc-setup.cmd` instead: a generated batch file that stages the virtio drivers from the
virtio-win disc. This is where the host's reach ends. The drivers that lift such a guest off a
fixed screen size and an emulated disk can only be installed from inside it, by a user with an
administrator token, so the last step is handed over as something the guest can run rather than
as something the reader must retype. The disc is built whenever either guest needs it, from one
predicate, so turning cloud-init off does not silently take the script away too.

**Installation is how the base comes into being.** A guest with no cloud image is installed by
`zvr install`, which takes flags rather than an app name on purpose: an app pins its base by
digest, and a disk that does not exist yet has no digest. Install first, pin the result, then
author the app against it. That keeps the rule that a pinned base is never written to intact.
The install seeds its machine identity from the disk's own path, because it has no app name
yet and a shared placeholder would give every install on every host the same identity at the
one moment it matters most.

### 10.5 The guest's network

A VM app declaring `NetworkLists` gets the same fail-closed egress a container does. qemu runs
inside a network namespace made by `pasta --config-net`, which gives it working connectivity and
uid 0 of a user namespace, so the ruleset loads with no privilege on the host. The ordering is the
guarantee: nft loads, and only then does qemu exec, so a guest never exists on an unfiltered
network - the same window `pod create` closes for a container.

The rules come from `common/domain/nftrules`, shared so that what a `NetworkList` MEANS cannot
differ between the two runtimes.

**The loopback hole, and what actually closes it.** pasta splices a namespace's loopback to the
host's, so a guest that could reach `127.0.0.1` would reach every service the person running it
has bound there. What closes that is the **input** chain's default drop, not the absence of a
loopback accept on egress: the splice works by pasta accepting the connection inside the
namespace. Measured three ways - a bare pasta namespace reaches a host loopback service, this
ruleset does not, and adding one `tcp dport <p> accept` to the input chain reaches it again.
Every accept in that chain is therefore loopback exposure on its port, which is why the only ones
are the published ports, and pasta itself binds those on the host.

**Forwards.** `ForwardPorts` is published by pasta rather than by qemu's `hostfwd`. A forward is
delivered to the namespace's interface address, so the guest's `hostfwd` binds every address of
the namespace rather than its loopback, where nothing arrives. That is narrower than it reads:
the only way in is a port pasta was told to forward, verified by an undeclared port staying
unreachable from the host.

**DNS is delivered, not only enforced.** qemu's user-mode networking takes its upstream resolver
from `/etc/resolv.conf`, so a guest whose lists are allowances would ask the host's resolver and
have its own rules drop the query - it resolved nothing. The namespace gets a resolv.conf naming
the declared servers, bind-mounted before qemu execs: no cooperation from the guest, so it works
for one with no cloud-init, and pasta's mount namespace does not propagate. `zc` warns when a
guest's lists are allowances and no resolver is named.

**Reading it back.** `zvr net <app>` reports what the ruleset has counted, as `zcr net` does for a
container, and the parser is shared with it. Posture is observed from the namespace the guest is
actually in rather than from its config, for the same reason `zcr net` reads pod membership: an
edited YAML must not change what is reported about a guest already running.

**What a guest does not get.** Only self-scoped egress. Sibling links, routing through a gateway,
forwarding and by-name allowances are refused at validation rather than half-applied, because a
guest has no siblings and no pod to link to. A guest that declares no lists at all is
*unfiltered* - it keeps qemu's user-mode NAT and reaches whatever the host can. That is the
opposite of a container with no lists, which gets `--network none` and is the most restricted
posture there is; `zvr net` says which of the two a running guest is in, in those words.

**What is not supervised.** Nothing watches a guest, so one shut down from inside leaves its swtpm
running, and `zvr` has no launch lock of the kind `zcr` uses to serialise a relaunch. Both are the
guest-side twins of container fixes, recorded rather than implied to work.

## 11. Host Surface (minimal)

Zinc adds almost nothing to the host. The moving parts:

| Component | Reason |
|-----------|--------|
| A Wayland compositor | owns the display; passes the socket in per app |
| A terminal emulator | drops into terminal/multiterminal apps on explicit launch |
| `zc` / `zcr` | two static binaries on `$PATH` - author and run apps |
| Rootless Podman + pasta | the container runtime and userspace networking |
| Pipewire (optional) | audio; the socket is passed in only on explicit grant, and it grants BOTH directions regardless of which the config asked for (see 5.9) |

Everything else runs inside containers. The host-side values a launch needs (Wayland and
runtime sockets, the theme bundle, the terminal emulator, the netfilter image) are resolved
from the environment in one place (the host adapter, section 13) into an explicit options
struct, so the argv-building code stays pure and testable. The environment variables:
`XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY`, `ZINC_TERMINAL` (else `$TERMINAL`), `ZINC_THEME_BUNDLE`,
and `ZINC_NETFILTER_IMAGE` (overrides the default helper image tag).

---

## 12. Desktop integration (ZDE)

Desktop hotkeys, a launcher, session profiles, and the Nix home-manager wiring belong to
**ZDE** (the Zinc Desktop Environment), a separate project layered on Zinc. They are not part
of the Zinc 0.1 core and are intentionally out of scope for this document. Zinc is
compositor-agnostic on purpose, so it can be adopted piecemeal on any existing system.

---

## 13. Repo Layout

The shared library is `common/`, which holds the app schema, its validation, and the
config-inheritance resolver and the WireGuard-config reader (`common/domain/schema`,
`.../validate`, `.../inherit`, `.../wgconf`). It is
pure - no I/O, and the resolver takes a loader from its caller rather than reading anything
itself - and its one dependency is the YAML codec, which the resolver needs because it merges
nodes rather than decoded structs (3.1). Both tools depend on it and validate identically. The runtime hexagon lives **inside**
`container/runner`; there is no separate shared runtime module.

```
zinc/
  Containerfile          generic reproducible build (any module; digest-pinned Go)
  check.mk               containerized checks (test/vet/fmt/vendor); every module includes it
  tool.mk                binary targets (build/run/repro); each tool's Makefile includes it
  go.work                ties the modules together for local dev only (the build never uses it)
  common/                shared library - schema, validation, inheritance, wg config (pure)
    domain/schema/                    schema.go (AppConfig, schema version 3)
    domain/schema/validate/           the hard rules + create-time warnings
    examples/apps/                    sample app YAMLs
  creator/               zc - the creator for BOTH app kinds (CLI + Bubbletea TUI)
    main.go                           CLI: authoring local; runtime routed by app Type
    delegate.go                       picks the runner: zcr for containers, zvr for VMs
    internal/store/                   the YAML app store (~/.config/zinc/apps)
    internal/runner/                  the runner delegate (finds zcr/zvr on $PATH)
    internal/backend/                 the one facade the CLI + TUI use
    internal/tui/                     the keyboard-first terminal UI
    internal/keys/                    the TUI keybind schemes (~/.config/zinc/zc)
    internal/compose/                 Compose-spec interop, both directions (9.1)
  container/
    runner/              zcr - the container runtime (the hexagon)
      domain/                         pure model: derived-image policy, launch options
      ports/                          interfaces: Store, Runtime, ImageBuilder,
                                        ImageResolver, NetEnforcer, + the neutral Command type
      app/                            launch orchestration (the Service)
      adapters/podman/                Runtime + ImageBuilder + ImageResolver (podman argv/exec)
      adapters/netenforce/            the NetEnforcer: pasta pod + nft ruleset  <- the swap point
      adapters/fs/                    the YAML app store + codec
      adapters/host/                  environment -> host launch options
      wire/                           composition root (assembles adapters -> app.Service)
      images/netfilter/               Containerfile for the nft helper image (make netfilter-image)
      main.go                         the CLI
    e2e/                 end-to-end tests: drive the real zc/zcr against podman
  launcher/common/       shared launcher library (store + zcr delegate + fuzzy matcher)
  launcher/tui/          zlt - the launcher TUI (fuzzy picker over the apps; shells out to zcr)
  launcher/gui/          zlg - the launcher GUI (thin consumer of menu/; shells out to zcr)
  menu/                  reusable Wayland overlay-menu core: layer-shell surface + software
                           renderer + keymap + theme + fuzzy picker. Pure-Go, cgo-free, and
                           depends on no Zinc module, so zde and others can import it (menu.Run)
  virtualization/runner/ zvr - the VM runner (the same shape as zcr)
    domain/qemu/                      pure: the qemu argv for a validated config
    domain/paths/                     where overlays, seeds and control sockets live
    adapters/machine/                 process supervision: start, find, stop a guest
    adapters/qmp/                     the guest control channel (status, power button)
    adapters/disk/                    overlay creation, digest verification, seed ISO
    app/                              launch orchestration (the Service)
  docs/architecture.md   this document
```

**The runner is a hexagon** (ports and adapters), so a mechanism can be swapped by writing a
new adapter rather than editing call sites - the motivating case being network enforcement
(5.3), where "not pasta" later is one more adapter. The layers:

- **`domain/`** - pure model and policy (the derived-image policy, the launch options type).
  No I/O, no podman/nft/fs/env.
- **`ports/`** - the interfaces the application depends on: `Store`, `Runtime`,
  `ImageBuilder`, `ImageResolver`, and `NetEnforcer` (the network swap point), plus the
  neutral `Command` type a `NetEnforcer` emits and a `Runtime` executes.
- **`app/`** - the application service that orchestrates a launch through the ports. The
  single launch path.
- **`adapters/`** - the concrete edges: `podman` (Runtime/ImageBuilder/ImageResolver),
  `netenforce` (the pasta pod plus nft ruleset - the swap point), `fs` (YAML store + codec),
  `host` (environment to options).
- **`wire/`** - the composition root: the one place that imports every adapter and assembles
  them into an `app.Service`. The CLI calls it; nothing else names a concrete adapter.

The shared schema (`common`) is vendored into each tool, so every per-module container build
stays hermetic (no network, no sibling checkout at build time). The repo-root `go.work` ties
the modules together for local editing only; the build never depends on it (`make vendor`
runs with `GOWORK=off`).

Two distinct container concerns live here: the repo-root `Containerfile` reproducibly builds a
**tool binary** (pinned Go toolchain plus that module's vendored deps); the netfilter
`Containerfile` under `container/runner/images/netfilter` builds the **runtime helper image**
the network lock-down applies rules with (6.4).

---

## 14. Known Issues and Tradeoffs

| # | Issue | Mitigation |
|---|-------|------------|
| 1 | Zinc supplies the security-context identity; what a tagged client is allowed to do is the compositor's policy, and a compositor without the protocol gets the raw socket and a warning | the container boundary is the real wall (5.1); a VM (section 10) is the stronger boundary for untrusted GUI apps |
| 2 | GPU passthrough weakens isolation, is granted unless a config opts out, and has no memory cap (see 5.4 on `dmem`) | `DisableGpuAccess: true` denies it; opt-out is deliberate, and it is the one grant not written into a config when it applies (5.4) |
| 3 | Image tags can be poisoned upstream | third-party images must be digest-pinned; launch is `--pull never` (5.5) |
| 4 | Derived images are per-machine, not digest-pinned | their guarantee is the pinned base plus the visible install lines (7) |
| 5 | Every schema field is enforced by something as of 0.10.0; what remains unenforced is named, not silent | the exceptions are listed in the changelog's Still open, and validation says so at authoring time |
| 6 | Host-scoped egress, gateway/multi-homing, and mixing a sibling link with other networking are unsupported | fail-closed: rejected at launch, never mis-enforced (6.5) |
| 7 | The netfilter helper runs with namespaced `CAP_NET_ADMIN` | namespaced to the pod's userns, harmless on the host; the image is local and `--pull never` (6.4) |
| 8 | A guest's egress covers self-scoped lists only; sibling links, gateways and by-name allowances do not reach it | fail-closed: refused at validation rather than half-applied (10.5) |
| 9 | Nothing supervises a guest: one shut down from inside leaves its swtpm running, and `zvr` has no launch lock | `zvr stop` tears down correctly; the gap is a guest that exits on its own (10.5) |

---

## 15. Status and Roadmap

**0.1 ships containers:** the schema and validation, the YAML app store, the full
`zc` + `zcr` split with a keyboard-first TUI, real rootless-container lifecycle, and the
fail-closed network lock-down (isolated / egress / LAN publish / sibling link).

**0.2 ships the launcher:** `zlt`, a keyboard-first fuzzy picker over the defined apps that
shells out to `zcr` (depends only on `common`, never imports the runtime).

**0.3 ships the GUI launcher** `zlg` over the reusable `menu` module, and **0.4 ships
virtualization**: `zvr` runs VM apps over qemu (section 10), and `zcc` became `zc` because one
creator now authors both app kinds. The ZDE desktop layer is a separate project in its
own repository (section 12), not a Zinc release. Within the container tools, the near-term
work is wiring the already-validated schema fields (resources, internal user) into the launch.
</content>
</invoke>
