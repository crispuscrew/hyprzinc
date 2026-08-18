# Changelog

All notable changes to Zinc are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Zinc uses
[Semantic Versioning](https://semver.org/spec/v2.0.0.html). The version line is
tracked in [RELEASES.md](RELEASES.md).

## [Unreleased]

### Changed

- **Schema v3: `Configs` is its own type and is finally mounted.** It was declared as
  `[]Volume`, where three of six fields could not apply to a single file and `HostMount`
  carried the opposite rule, and it produced no mount at all. Now `[]ConfigFile` with
  `BundlePath` (relative to the app's bundle), `InnerMount` and `Writable` (read-only by
  default). Mounted from `$XDG_CONFIG_HOME/zinc/apps/<app>/configs/`, per app rather than per
  instance. Placeholders in a `BundlePath` are refused rather than expanded.

- **Schema v3: `Env` and `ReadOnlyRootfs`.** There was no way to set an environment variable
  at all, since `Install` becomes a `RUN` layer rather than an `ENV`. `Env` is a mapping, so a
  duplicate key cannot be written, and it is emitted sorted because `--dry-run` output must be
  stable. `XDG_RUNTIME_DIR`, `WAYLAND_DISPLAY` and `DBUS_SESSION_BUS_ADDRESS` are refused: they
  describe what the runner built. `ReadOnlyRootfs` maps to `--read-only`, with the tmpfs
  mounts left writable. Both are refused for VM apps.

- **Schema v3: `DisplayMeta.RequireSecurityContext`.** A compositor without
  `wp_security_context_v1` gets handed the raw socket, labelled `zinc.wayland=passthrough`.
  Nothing could ask for the strict answer; now a config can, and the launch is refused instead.
  Setting it with `DisableSecurityContext` is refused, as is setting it on a VM app.

- **Schema v3: `AudioMeta.Monitor`, the third audio capability.** A PipeWire sink carries a
  `.monitor` source, so a client on the socket can record what OTHER apps are playing - a
  capability that crosses the boundary between two sandboxed apps. Takes `none` or `default`
  only, since no `/dev/snd` node carries a monitor source. Like `Microphone: none`, `none` is
  not yet enforced for a container and validation says so.

- **Schema v3: audio is granted one direction at a time.** `Pipewire` and `LegacyALSA` are
  replaced by `Playback` and `Microphone`, each taking `none`, `default`, or a list of
  `/dev/snd` nodes. `Pipewire: true` mounted the session socket, which grants capture and the
  monitor sources along with playback, so every app that wanted to make a sound could listen to
  the room; on the VM side `hda-duplex` was attached unconditionally.

  The three forms are enforced differently, which is why they are spelled differently: a device
  list is passed with `--device` and enforced by the kernel; `default` on a VM is enforced by
  qemu (`hda-output` has no capture stream); `default` on a container is **not** enforced yet,
  and `zc` warns on exactly that case. Closing it means speaking PipeWire's security context.

  **Migration.** `SchemaVersion` becomes 3. `Pipewire: true` becomes `Playback: default`, plus
  `Microphone: default` if the app records. `LegacyALSA: true` granted all of `/dev/snd` and has
  no direct equivalent: name the nodes. Unknown keys are rejected, so a v2 config fails loudly.

### Security

A second audit, aimed at where 0.9.1 did not reach: the VM disk chain, the paths that load a
config without validating it, and the tools around the runner. Two findings share a root cause:
a pin covers the bytes of a file, and a file can point somewhere else.

- **A pinned VM base image could hand the guest any file the user can read, or a URL.** A qcow2
  `backing_file` pointer is inside the digest, so a hostile image pins perfectly while the guest
  boots whatever it resolves to; with the nbd or curl drivers present it can be a URL. An image
  declaring a backing or external data file is now refused as not self-contained, checked before
  the digest so it also applies on the cached path.

- **`zcr stop` and `zcr restart` on a config given by path never validated it.** `AppNameID: --all`
  turned the teardown's `podman rm -f --ignore <app>` into `--ignore --all`, and `../..` walked
  its `rm -rf` out into the session runtime directory. Every verb that loads a config now
  validates the name there, and the socket-directory helper refuses a path outside its own root.

- **A WireGuard `Endpoint` could carry shell metacharacters into the privileged helper.** The
  hand-rolled address test accepted any colon-bearing string without a slash or space, so
  `[::$(cmd)::]:51820` reached the tunnel script the helper runs with `CAP_NET_ADMIN` before the
  ruleset closes the namespace. It goes through `net.ParseIP` now, like its siblings.

- **`Talk: ["org.freedesktop.*"]` was accepted, unwarned.** That subtree contains
  `org.freedesktop.systemd1`, whose `StartTransientUnit` runs an arbitrary command outside the
  container. A wildcard must now name at least three elements before the `*`. `zc` additionally
  warns on a grant naming a service whose purpose is to run code.

- **A config could mount the raw session bus and the raw compositor socket.** A volume naming
  `/run/user/1000` handed the app both while `DBusMeta` stayed empty and every Zinc surface
  agreed with it. Sources under the runtime directory, `/proc` and `/sys` are refused.

- **`zvr install` validated nothing.** `--media` and `--disk` went straight into `-drive`, where
  a second `file=` in the tail replaces the path. Both are screened now, and `--firmware`,
  `--devices` and `--secure-boot` are checked against their enums rather than silently
  reinterpreted as BIOS-with-virtio.

- **An unverified UEFI variable store could decide what Secure Boot enforced.** The store beside
  a base image is adopted on first run and nothing pinned it, so `SecureBoot: true` ran the
  secure-boot firmware over an attacker-chosen PK/KEK/db. No attacker needed either: install
  without `--secure-boot`, author with `SecureBoot: true`, and the guest boots in setup mode.
  Adoption is refused when `SecureBoot` is set.

- **Host mount and key paths must be absolute.** Podman resolves a relative source against its
  own working directory and reads a separator-less source as a named volume it creates. A `Keys`
  path additionally could not contain `..`, since `Path: /..` mounted the host filesystem over
  the container home.

- **`ImageMeta.Image` only had to end in something digest-shaped**, so
  `-v/:/host@sha256:<64 hex>` validated as pinned and reached podman in a bare positional slot.
  Anchored at both ends now.

- **`ZINC_NETFILTER_IMAGE` was passed through unchecked**, holding the most privileged image
  Zinc runs to a weaker standard than an app's own. It must be a `localhost/` reference or a
  digest pin, and the default is fully qualified.

- **`zvr stop` could signal an unrelated process.** The swtpm check matched "swtpm" anywhere in
  a recycled pid's command line. It compares argv elements now.

- **`make virgl-venus` built from a mutable tag**, and `zvr` printed the raw clone-and-build
  sequence as the fix. The commit is pinned and checked, and the hint points at the make target.

### Fixed

- **`Autorestart` produced an argv podman refuses.** `--rm` and `--restart` conflict at the CLI
  layer, and the launch is detached with no stdio, so `zcr` reported success while the app never
  started and the pod, ruleset, proxy and holder were left behind. `restart: always` in a compose
  file imports straight to this.

- **The Wayland holder never let go of a container that runs without `--rm`.** It waited for
  removal, which never happens for a `KeepAlive` or `Autorestart` app, so it span every 250ms for
  the session and the context was never revoked. It asks whether the container is running again,
  with a restart window.

- **`zc tui` acted on the name a config claimed rather than the file it came from.** A dropped
  `notes.yaml` saying `AppNameID: firefox` made delete remove the real `firefox.yaml` while
  reporting success. Every action uses the store key now, and the list displays it.

- **`zc` had none of the exec-boundary guards the launcher has**, so a dropped `--net=host.yaml`
  became a runnable row and `notes.yaml.yaml` listed as `notes.yaml`, which `zcr` re-reads as a
  relative path. Both guards now exist on both sides.

- **The app list could be rewritten by a config.** Fields are painted unescaped, so a newline in
  `ImageMeta.Image` added a fabricated row and a cursor escape repainted the row above.
  Validation never runs on the display path. Control characters are stripped and fields bounded.

- **The bus socket directory was 0700 only at its leaf**, since `mkdir -m 700 -p` leaves the
  parents at the umask.

- **`safeName` in the creator refused a legal app name.** It tested `..` as a substring, so
  `my..app` could be created and never deleted again. It compares path segments now.

### Changed

- **The GPU default is documented honestly.** `/dev/dri` is granted unless a config sets
  `DisplayMeta.DisableGpuAccess`; the architecture doc said twice that it was off by default.
  The code was always the authority. Opt-out stays, since almost every graphical app needs the
  GPU: it is the one grant whose zero value is permissive.

- **`flake.lock` is committed.** The flake pinned nixpkgs by commit but shipped no lock, so
  nothing recorded what was actually built. CI builds with `--no-update-lock-file`.

- **The virtio-win driver ISO no longer claims to be verified.** The check confirmed two files
  began with `MZ`, which is a corruption check, and the ISO comes from a floating path with no
  digest. The output now says which of the two it did and prints the digest to pin.

### Still open

- A VM app has no egress control: `-netdev user` is unconditional and `ForwardPorts` only adds
  inbound entries, so every guest gets unrestricted outbound plus the host's loopback through
  slirp's gateway. This belongs to `zvr`, not the schema. pasta is already a dependency and can
  back a qemu netdev, which would give both runtimes one network model.
- No VRAM limit exists, deliberately. The mechanism is the kernel's `dmem` cgroup controller
  (6.14+), which needs the DRM driver to register regions; on the development box the controller
  is present while `dmem.capacity` is empty. A field now would read as a cap and do nothing.
  Additive when regions exist, with no schema bump.
- The proxy, pod, netns, bridge and published ports are not torn down when an app exits on its
  own: the reaping goroutine cannot run, since every front-end launches through a short-lived
  `zcr`. The leaked pod also makes the next filtered launch fail, `pod create` having no
  `--replace`. The fix is a supervisor that outlives the app, which the Wayland holder already is.
- The already-running refusal reads `podman ps` at the top of the launch, but the container does
  not appear there until the end, so a second launch a second later still passes and its
  fail-closed teardown removes the first one's pod and proxy. There is no lock in the launch path.
- `zcr net` reports posture from the config file at report time without probing the running
  system, so editing a YAML changes what the attestation surface says about a running app.
- A config run from a file path can claim an `AppNameID` that resolves to another app's address,
  forging that app's Wayland `app_id`, container name, bus row and `zcr net` posture.
- Every guest reaches host services on 127.0.0.1 through user-mode networking's gateway.
- Release tags are unsigned and there are no published checksums.
- The multiterminal launch enforces in a detached process with stdio discarded, so a failed
  ruleset load or a rejected security context is reported as a successful launch.

## [0.9.1] - 2026-07-31

A whole-repo audit, every finding verified against a running system rather than by
reading. 22 defects fixed. Two of them are the reason this is a release and not a
housekeeping commit.

### Security

- **A wg-quick file could run arbitrary commands in the privileged helper.** `Address` and
  `AllowedIPs` were spliced unquoted into the `sh -c` script the netfilter helper runs, and
  that helper holds `CAP_NET_ADMIN` in the app's network namespace and runs BEFORE the egress
  ruleset is loaded. A `.conf` from a VPN provider - the artefact this feature exists to
  consume - could therefore execute as uid 0 with the namespace still unfiltered. Demonstrated
  end to end: an injected payload installed its own nft accept rule and the app then reached
  the internet despite an allow list naming only its tunnel endpoint.

  `wgconf` already refused `PostUp` and `PreUp` because "a config file is not a place to accept
  code from". These two fields were the same hole under a different name. Every entry must now
  parse as an address or a network.

  **Note the layer:** `zcr validate` does not read the referenced file, so it still reports
  `ok`. The refusal happens at launch, where the file is actually parsed, with a line-numbered
  error naming the offending value.

- **The nftables load was additive, not a replacement.** `nft -f -` merges into whatever is
  already in the namespace, so a rule that was there first evaluated ABOVE everything Zinc
  wrote. The ruleset is meant to be a function of the validated config alone; it was a function
  of the config plus whatever got there first. Both tables are now cleared in the same atomic
  transaction that installs them.

- **A tunnel app had no `input` base chain, so inbound over `wg0` was unfiltered.** nftables
  applies no policy to a hook with no base chain, which the code comment three lines above the
  bug states outright - it just did not list the tunnel among the cases needing one. With
  `AllowedIPs = 0.0.0.0/0` the peer could address any port on the app. `zc new --tunnel`
  authors exactly this shape, so it was the common case.

- **`iif "lo" accept` bypassed a publish's source filter.** pasta delivers a connection
  forwarded from the host's loopback into the namespace over `lo`, so the rule intended for
  "the app talking to itself" took those before the source-CIDR rules were consulted: a publish
  naming one remote /24 was reachable by every process on the host. Now scoped to loopback
  addresses, so a spliced connection falls through to the rules that decide about it.

- **A comma in a VM image or `InstallMedia` path injected qemu drive options.** qemu separates
  `-drive` properties with commas and resolves a duplicate key to the last one, so a second
  `file=` in the tail replaced the absolute path validation had approved - and qemu will open a
  URL. Since `zvr install` boots from that medium, the boot disk could be made remote, mutable
  and unauthenticated, which is exactly what `BaseDigest` exists to prevent for the main disk.

- **`zvr stop` signalled whatever pid was in the pidfile**, with no `/proc` check, so a
  recycled pid meant terminating an unrelated process. `State` already applied that check and
  the TPM code cites the supervisor as its precedent; the supervisor was the one place not
  doing it. The cmdline match is also now an exact `-name <app>` argv comparison rather than a
  substring search that any guest's own overlay path could satisfy.

- **`zvr reset` and `zvr stop` took an unchecked name into a path join.** `filepath.Join`
  cleans `..` away rather than refusing it, and `reset` deletes an overlay, a seed, a UEFI
  variable store and recursively a TPM state directory - a delete primitive aimed at any path
  the user can write, reported as success for an app that was never defined.

- **An unparseable `DBUS_SESSION_BUS_ADDRESS` fell back to the main user bus.** The function's
  own comment promised the opposite ("anything else resolves to empty ... which is the
  fail-closed answer"). A user who pointed the session at a nested or restricted bus got a
  sandbox proxied onto the main one instead, silently. It also split on `,`, which separates
  key-value pairs inside one address, not the addresses themselves.

### Fixed

- **Launching an app that was already running tore down the running app.** The second launch
  failed on its first prepare step, and its fail-closed teardown then removed the pod, D-Bus
  proxy and socket directory belonging to the first, healthy launch. A double Enter in a
  launcher killed the app the person was working in; two concurrent launches left nothing
  alive. A launch now refuses before it prepares anything.

- **The Wayland holder revoked the security context on the first container stop.** `podman
  wait` returns on every exit, including one podman is about to undo, so a `zcr restart` or an
  `Autorestart` app's first crash revoked the context and the container came back bind-mounted
  onto a socket the compositor no longer accepts on - no display at all.

- **A `KeepAlive` app could never be started again after `zcr stop`.** It runs without `--rm`,
  so `stop` left an Exited container holding the name and every later launch collided with it.
  The unfiltered teardown now removes rather than stops.

- **`zcr logs`, `zcr inspect` and unfiltered `zcr restart` never worked for `app@instance`.**
  They passed the raw argument to podman instead of the resolved runtime name.

- **A leaked D-Bus proxy blocked every future launch of its app.** The proxy container is now
  created with `--replace`, since Zinc owns that name by construction.

- The D-Bus socket directory is created `0700`, matching what the Wayland side already argued
  for; it was inheriting the helper image's umask.

### Changed

- **`make check` is hermetic, not merely containerized.** The check containers now run with
  `--network=none` and `--cap-drop=ALL`. `go test` runs arbitrary test code, so with a network
  a test could quietly depend on fetching something and still pass.

  This immediately exposed a test that had only ever passed because the container ran as root:
  it made a file `0o000` to prove a cached digest verification never reopened it, but the chmod
  moves ctime, which is part of the cache identity, so the cache always missed and
  `CAP_DAC_OVERRIDE` reopened the file anyway. The assertion was vacuous wherever it passed and
  failed for any ordinary user.

- **The Nix flake is pinned to a nixpkgs revision** rather than tracking `nixos-unstable`. CI's
  own logs showed nix creating a lock file on every run and resolving the branch to that day's
  HEAD, so one Zinc tag could build against different compilers on different machines.

- **`make build` no longer reports success when the binary extraction failed.** Three commands
  chained with `;` meant the recipe's exit status came from the cleanup, so a failed `podman cp`
  printed "built" and left the previous binary in place - which the e2e harness then accepts as
  current, because it only builds what is missing.

- The CI `common` job runs through the pinned container with `vendor-check`, instead of a host
  Go that floated to whatever 1.24.x was newest. The e2e base image and the netfilter helper's
  remaining packages are digest- and version-pinned.

### Still open

The audit found more than was fixed. These are recorded rather than quietly dropped, and each
needs the same reproduce-then-fix treatment: the D-Bus proxy has no watcher, so it is not torn
down when an app exits on its own; `zcr run --exec` leaks a pod when the app dies post-fork;
dependencies bypass mount templating and the VM-app refusal; `{state}` directories are never
created, so the `zc init` example does not run; the `$EDITOR` round-trip in `zc tui` can
silently restore a D-Bus grant the user narrowed; `zinc-dbus-<name>` is a legal app name and
can forge an attribution row; and an app name ending in `.yaml` is re-read as a path.

## [0.9.0] - 2026-07-31

The last three of ZDE's seven asks, which closes the list. All three are about the same
thing from different angles: a sandbox nobody outside can inspect is one nobody outside can
trust. 0.8.x made an instance addressable; this makes it attestable.

The Wayland change is the substantive one. Until now Zinc labelled the container and handed
over the compositor's own socket, so a desktop had nothing to go on but what the app said
about itself. It now creates a real security context, and the identity the compositor holds
is the one Zinc constructed. The same principle drives the other two: bus attribution
publishes the mapping Zinc already holds rather than asking the app to claim a name, and
`zcr net` reports what the ruleset counted rather than what it was configured to do.

### Added

- **Bus attribution: `zcr bus [--json]`, and `zcr where` now reports the bus.** Given
  something observed on the host session bus, a desktop can now say which Zinc app and
  instance it is. `zcr bus` prints every running D-Bus proxy with its host pid and the
  `app@instance` it serves; `zcr where` gains the app's filtered socket and proxy container
  (`none`, or JSON `null`, for an app that asked for no bus). Both have a `--json` form whose
  exact shape is pinned by tests, since it is what a desktop scripts against.

  The mapping is one Zinc already holds by construction - it creates the proxy container and
  names it after an app it has resolved - so nothing is taken from the app. The rejected
  alternative is worth recording: having each app own a name like `zinc.app.<app>.<instance>`
  cannot work, because `xdg-dbus-proxy` is a relay with no bus identity of its own and
  `--own=` merely grants THE APP permission to claim that name. The app would then have to
  claim it, which is a self-assertion - the thing attribution exists to stop trusting.

  Honest about the links: connection to pid is the bus's own `SO_PEERCRED` answer, and proxy
  container to app is the name Zinc gave it, both solid; pid to proxy is read live and is
  best-effort across time, since a dead proxy whose pid was recycled would be attributed to
  whoever holds the number now. Measured, and worth knowing before reading an empty answer as
  "no bus": the proxy opens one upstream connection PER CLIENT, so an app with no live bus
  client has no host-bus connection at all, and one with several has several - all on the
  proxy's single pid.

  **Observable:** with an app running, `zcr bus --json` lists it; asking the host bus for a
  connection's `GetConnectionUnixProcessID` and looking that pid up in the table names the
  app. The end-to-end suite walks exactly that chain against the real session bus.

- **`zcr net` - seeing what the lock-down actually did.** A firewall nobody can inspect from
  outside is one nobody can trust. One verb answers both halves: `zcr net` lists every running
  app with its network posture, and `zcr net <app[@instance]>` reports what that app's ruleset
  has counted. `--json` gives either as a machine-readable document, since the consumer is a
  desktop shell.

  The two postures are never merged. `filtered` means the app has `NetworkLists`, so a pod
  netns of its own with the nft ruleset locked in it; `isolated` means it has none, so
  `--network none` - it reaches only its own localhost and has no netns or ruleset at all. An
  isolated app is not a filtered one whose counters happen to be zero, and showing them alike
  would say the opposite of what is true about one of them. Apps are named the way a person
  types them (`app@instance`), never the runtime form; anything running that is not a defined
  app - Zinc's own D-Bus proxies, a container the user started - is left out rather than
  listed with a posture Zinc cannot vouch for.

  The read takes the same route as the lock-down: same helper image, same pod, same one
  capability, `nft -j list ruleset` instead of `nft -f -`. Reading nftables is not a lesser
  privilege than writing it, so there is no weaker path to take; it joins the pod's network
  namespace and nothing else (a pod shares net, ipc and uts, not pid), and exits as soon as
  the dump is written.

  **Observable:** `zcr net` shows `netprobe@work  filtered  netprobe.work-pod` beside `quiet
  isolated  -`; three connections to an allowed destination and one query to an undeclared
  resolver read back as `list[0] ip tcp 3 180` and `undeclared dns udp 1 57`.

- **Counters on the rules that decide something.** The generated ruleset now carries a
  `counter` and a naming `comment` on each rule a `NetworkList` produced (labelled `list[N]`
  by its index in the config, so a number points at the line that produced it), on the link
  and tunnel accepts, and on the DNS deny. Deliberately not on `oif "lo"` or `ct state
  established,related`: between them they are almost all the traffic and neither answers a
  question about policy.

  Every default-drop chain also gets a trailing `counter drop`, because nftables counts rules
  and not policies - a fail-closed chain refuses by policy, so the most valuable number of all
  would otherwise be permanently zero whether the lock-down worked or not. It changes no
  behaviour, and is emitted only for a drop policy: on an all-blacklist chain the same line
  would turn allow-all-except into deny-all.

  Because the conntrack accept stays bare and stays above them, an accept counter counts the
  packets that OPENED flows rather than the traffic they carried - the more useful reading,
  and why the byte column is small.

  **Counters are not lifetime totals.** They live in the pod's netns and are created with it,
  so `stop`, `restart` and any failed launch reset them to zero. The output says so in both
  forms rather than leaving it to be assumed.

- **A real Wayland security context per app instance.** Zinc no longer just labels the
  container and hands over the compositor's own socket. It connects to the real compositor,
  binds a socket of its own under `$XDG_RUNTIME_DIR/zinc/wayland/<instance>/`, and registers
  it with `wp_security_context_v1` before the app container is created. The app mounts that
  socket at the same container-side path with the same `WAYLAND_DISPLAY`, so nothing inside
  the app changes - but every connection it makes is tagged by the compositor with an
  identity the app cannot forge.

  The identity is `sandbox_engine=com.github.crispuscrew.zinc`, `app_id` = the app name
  (stable across instances, as the protocol requires) and `instance_id` = the runtime name,
  which is deliberately the same string that names the podman container and that `zcr where`
  reports - so a compositor holding an `instance_id` can find the container behind the window
  instead of holding an opaque uuid.

  A hidden `zcr __wayland <app[@instance]>` process holds the context open for the app's
  lifetime and revokes it when the container is gone, the same shape `__term` uses, because
  `zcr run` detaches and a context is revoked by closing a descriptor.

  **Observable:** `podman inspect <app> --format '{{.Config.Labels}}'` reports
  `zinc.wayland=security-context`, its bind is the derived socket, and a client connecting
  through it sees a restricted global set (measured: 39 globals versus 62 on Hyprland 0.51.1,
  29 versus 42 on niri 26.04, with `wp_security_context_manager_v1` withheld in both).

### Changed

- **`zcr where` requires the app to be defined.** It now loads the config, because whether
  there is a bus socket to report is a fact about the config; guessing for an unknown name
  would report a path with the same confidence as a real one. A VM app is refused there for
  the same reason every other `zcr` command refuses one.

- **The `zinc.wayland` label is now honest about which mode happened.** It was applied
  whenever an app had not opted out, which claimed a security context on every launch
  including the ones that never created one. It is now `security-context` or `passthrough`,
  always present when a Wayland socket is mounted at all. A label that is wrong is worse than
  no label - it is what a desktop reads to decide how much to trust a client.

- **A compositor without `wp_security_context_v1` falls back to the raw socket with a warning
  on stderr** rather than failing the launch, so Zinc stays usable on desktops that have not
  adopted the protocol. Every other failure - a socket that cannot be bound, a compositor that
  cannot be reached, a rejected request - fails the launch instead.

## [0.8.2] - 2026-07-31

Work driven by what ZDE is blocked on. Five of its seven asks; the two still open are
noted at the end.

### Added

- **Instances: one definition, many running things.** `zcr run app@instance`, or
  `--instance NAME`. Two instances coexist and tear down independently; an app with no
  instance keeps the exact container name it has always had, so nothing existing is renamed.

  `@` is the address a person types and `.` is the runtime name, because podman refuses `@`
  outright. Instance names are `[a-z0-9][a-z0-9_-]*`. The instance is applied in one place -
  the load path every command already goes through - so the pod, container, healthcheck,
  D-Bus proxy and teardown all follow without a second identifier threaded through each
  adapter, and without one adapter quietly sharing a pod between instances meant to be
  separate.

  **Observable:** `zcr run sleeper@work --exec && zcr run sleeper@personal --exec` yields
  containers `sleeper.work` and `sleeper.personal`; stopping one leaves the other running.

- **`zcr where <app[@instance]>`** prints where an instance keeps its state and what its
  container is called. State is under `$XDG_STATE_HOME` (falling back to `~/.local/state`)
  at `zinc/<app>/<instance>`. It exists so nothing outside Zinc hardcodes the layout: a
  second copy of the rules drifts the first time either side changes. It answers whether or
  not the instance is running, which is why it is not part of `inspect`.

- **`{state}`, `{app}` and `{instance}` in host mount paths**, so one definition serves many
  desks without a copy per desk. `{state}` expands to exactly what `zcr where` reports, and a
  test pins that they cannot disagree. Only the host side is templated: the container side is
  what the app looks at, and it is the same for every instance by design. An unknown
  placeholder is an error, never a literal directory.

- **`ImageMeta.SourceTag` and `zcr recheck <app>`.** A digest alone cannot be asked whether
  it is stale - a rebuilt tag and an abandoned one look identical once pinned. SourceTag
  records what the digest was resolved from; `recheck` re-resolves and compares. It changes
  nothing, because re-pinning is a decision with a human behind it. Exit status carries the
  answer for tooling: 0 current, 1 error, 2 moved.

- **`zc init`** seeds a fresh store with three worked examples - a terminal app, an
  egress-locked app, and an instanced app with a `{state}` mount. A new machine had an empty
  apps directory and nothing to look at.

### Still open

- A real `wp_security_context_v1`. Zinc applies a podman *label* and passes the raw
  compositor socket, so the compositor learns nothing from Zinc about which app a window is.
  Giving a desktop a trustworthy per-instance `app_id` means Zinc speaking the protocol and
  handing over the derived socket.
- Bus attribution. `xdg-dbus-proxy` is a relay with no bus identity of its own, so having it
  "claim a name per instance" is not available: `--own` grants the *app* permission to claim
  a name, which is a claim again. The workable shape is Zinc exposing the mapping it already
  knows by construction.

## [0.8.1] - 2026-07-31

### Added

- **Nix flake and home-manager module.** `flake.nix` exposes each tool as a package (`zc`,
  `zcr`, `zvr`, `zlt`, `zlg`), a `default` joining all five, and `homeModules.zinc` putting
  the selected ones on PATH. `programs.zinc.tools` defaults to `zc`/`zcr`/`zlt`; `zvr` and
  `zlg` are opt-in, since a VM runner without qemu on the machine cannot work and a desktop
  shipping its own launcher does not want a second one. `homeManagerModules` is an alias, as
  consumers may pin either name. nixpkgs is the only input, so a consumer overrides one
  input rather than three.

  This is a second way to build the binaries and does not replace the gate: `make check` and
  `make repro` in the digest-pinned container still decide whether a change is good. The
  flake is for consumers.

  **Observable:** `nix run .#zc -- version` prints the flake's version. CI builds all five
  tools through the flake and runs one, because five green derivations proved nothing the
  first time - the binaries were installed under their module names, so `bin/runner` was
  written twice and `bin/zc` did not exist.

- **Instance addressing, and `zcr where`.** `app@instance` is the address a person types.
  It cannot be the runtime name (podman refuses `@`), so `@` is the human form and `.` the
  runtime form. An app with no instance keeps the bare name it has always had, so nothing
  already running is renamed. Instance names are `[a-z0-9][a-z0-9_-]*`: no dots, since the
  dot is the separator, and no uppercase, so an address echoes back as typed.

  Per-instance state lives under `$XDG_STATE_HOME` (falling back to `~/.local/state`, that
  variable's own default) at `zinc/<app>/<instance>`.

  **Observable:** `zcr where <app[@instance]>` prints the resolved state directory and the
  runtime container name, as two labelled lines whose labels are the contract:

      $ zcr where firefox@work
      state: /home/u/.local/state/zinc/firefox/work
      container: firefox.work

  It exists so nothing outside Zinc hardcodes the layout - a second copy of the rules drifts
  the first time either side changes. It answers whether or not the instance is running,
  which is why it is a command of its own rather than part of `inspect` (a passthrough to
  `podman inspect`).

  Addressing and layout only: `zcr run --instance` does not exist yet, so nothing threads an
  instance through a launch.

## [0.8.0] - 2026-07-30

### Added

- **Per-instance filtered D-Bus session bus (`DBusMeta`).** An app gets no session bus at all
  unless it names one, which is the default worth keeping: the host bus reaches the keyring,
  the portal, the compositor and every other service the user runs, so mounting it into a
  sandbox undoes most of the sandbox. `Talk` lists names the app may call, `Own` names it may
  claim. `zcr` runs `xdg-dbus-proxy` in the helper image, in a container it owns, holding the
  real socket and serving the app a filtered one.

  The proxy is deliberately not a member of the app's pod: a pod shares the PID namespace, so
  a proxy inside it would be a process the app could signal or ptrace. Measured against a live
  bus, an app granted one name sees 2 names through its socket where the host bus has 77 - the
  ungranted ones are invisible, not merely denied.

  Authored from either surface: `zc new --dbus-talk a.b.C --dbus-own a.b.C`, or the `dbus.talk`
  and `dbus.own` rows in the TUI. Both set `InternalUserMeta.KeepUserID`, which a filtered bus
  requires, and say so - validation refuses the pair without it rather than having the runner
  change who an app runs as at launch. `DBusMeta` on a VM app is a validation error.

- **Apache License 2.0.** The project had no licence file at all.

### Changed

- **CI pins its runner image and picks a working OCI runtime.** Every job drives podman, so the
  runner's podman/crun pair is part of the build toolchain. `ubuntu-latest` moved it twice in
  three days: podman 4.9.3 not understanding the `--health-cmd` exec form, then podman 5.8.4
  against a crun that rejects the OCI spec version it emits, which failed `podman run` outright
  before any Go ran. The image is pinned, and a pre-flight script probes the runtime and falls
  back to runc when the default cannot start a container.

- **Dropped `llms.txt`.** Nothing consumed it, and it was a fourth copy of the project
  description that had already gone stale. `CONTRIBUTING.md` is the contributor entry point.

### Fixed

- **`InternalUserMeta.KeepUserID` on an app with any `NetworkList` could not start at all.**
  Shipped broken in 0.7.0: the flag went on the container, podman refuses `--userns` on a
  container joining a pod, and because `StartApp` is detached the refusal went nowhere - the
  app simply was not there. `zc validate` said ok and the printed plan looked right.
  The user namespace belongs to the pod, so that is where it goes now. That alone was not
  enough: in a keep-id pod the privileged helpers ran as an ordinary uid and nftables could
  not touch the namespace (`cache initialization failed: Operation not permitted`), which
  would have meant an app keeping its uid could not have a lock-down. They now run as
  `--user 0` - root *of the pod's user namespace*, which is what owns the netns, and a no-op
  for a pod that is not keep-id. Verified: the app comes up at uid 1000 with its
  default-drop ruleset loaded.

## [0.7.0] - 2026-07-28

Containment, sibling routing, and interop. The 0.6 line finished the guest story; this one
goes back to containers and closes the gap between what an app config *said* and what the
runtime actually did - starting with two fields that had been validated and ignored since
0.1, and ending with an app that can hold a WireGuard tunnel while holding no capabilities.

### Added

- **`zc new --tunnel <wg.conf>`: author a tunnel app in one command, and author it working.**
  It reads the config and adds the egress rule the handshake needs - UDP to each peer's
  endpoint - rather than only setting the path. Setting the path alone produces an app that
  cannot work and does not say why: the tunnel is built inside a namespace whose ruleset
  default-drops, so without that rule the handshake never leaves and the interface carries
  nothing. The endpoint is already in the file, so nobody has to copy it across and the two
  cannot disagree. A config Zinc could not apply is refused here, at authoring time, rather
  than at the first launch. Verified by launching what it wrote: live handshake, payload
  fetched through the tunnel, and the app at `CapEff: 0000000000000000`.
  The WireGuard config reader moved to `common/domain/schema/wgconf` so both tools can use
  it - `zc` authors from it and the runner applies it, and `zc` may not import the runner.

- **`NetworkMeta.Tunnel`: Zinc builds an app's WireGuard interface, so the app never holds
  the capability that builds it.** A tunnel needs `CAP_NET_ADMIN` to create, and an app with
  `NetworkLists` may never hold it - `NET_ADMIN` in the pod netns would let the app flush the
  ruleset that contains it, so validation refuses the pairing. The consequence, until now,
  was that a "VPN container" could only ever be a NAT hop: nothing in Zinc could be an actual
  tunnel endpoint.
  Point `WireGuardConf` at a wg-quick-format config and the runner creates `wg0` in the app's
  netns, applies it, assigns the addresses and routes the peers' `AllowedIPs` into it - in
  the same privileged helper that already installs routes and loads the ruleset, and before
  the app exists. Measured end to end against a real WireGuard peer: handshake completed,
  pings across the tunnel, and the app reporting `CapEff: 0000000000000000` - including the
  `AllowedIPs = 0.0.0.0/0` case, where the default route points into the tunnel and the
  handshake can only complete because the endpoint was pinned to its pre-tunnel route first.
  And through a gateway: a client routed at a tunnel-carrying sibling fetched a payload that
  lives only on the far side of that tunnel, which takes the `Via` route, the forward chain,
  the masquerade onto `wg0`, the tunnel itself and the return path all working at once.
  The private key travels on the helper's stdin, the channel the nft ruleset already uses -
  not an argv, which every process on the host can read out of `/proc`, not an image, not a
  mount the app could open. Each peer endpoint is pinned to its pre-tunnel route first, or an
  `AllowedIPs = 0.0.0.0/0` config would send the encrypted packets carrying the tunnel into
  the tunnel; that is also why an `Endpoint` must be an address rather than a name, since the
  pin happens before the namespace closes.
  wg-quick's script directives are refused rather than run - `PostUp`, `PreUp`, `PostDown`,
  `PreDown`, `SaveConfig`, `Table`. The helper that would run them holds `NET_ADMIN` in the
  app's namespace, and a config file is not a place to accept code from. `DNS` is refused
  too, because `NetworkMeta.DNSServers` is already the one place to say it. The parser rejects
  unknown keys outright: this file decides what an app can reach, and a typo in it must not
  quietly widen or narrow that.
  The netfilter helper image gains `iproute2` and `wireguard-tools` (busybox `ip` cannot
  create a wireguard link).

- **`NetworkList.ForwardPorts`: a gateway bounds what it carries.** A forwarding app's
  `forward` chain was a blanket accept - `iifname zlink0 oifname zegress0 accept`, no
  destination, no port - so a client routed through a gateway reached anything on any port.
  A gateway can now say what it will pass: `ForwardPorts: [53]` makes it a DNS hop, and only
  port 53 crosses it whatever a client points at it. Empty carries any port, which is what a
  general-purpose gateway is for.
  **Only the ports, deliberately.** The bound has two ends and they belong to different apps:
  *where* is the client's, since only the CIDRs its own `Via` list names are routed to the
  gateway at all and it cannot change that - the runner installs those routes and the app has
  no capability to alter them - and *what* is the gateway's. Repeating the addresses on the
  gateway would let the two disagree. A gateway's own egress rules still do not bound what it
  forwards, and that is now stated in the architecture doc as a design line rather than left
  to be discovered: they say where *this app* may go, and forwarded traffic is somebody
  else's.
  Measured twice: a client whose only path is a `ForwardPorts: [53]` gateway resolved
  `example.com` through it and was blocked connecting to port 443 of the address it had just
  resolved; and through a tunnel-carrying gateway with `ForwardPorts: [8080]`, a client
  fetched the far end's 8080 and timed out on its 9090.

- **Gateways chain.** The forward rule named the egress bridge alone, so a gateway that was
  itself routed through another sibling dropped everything its own `Via` routes sent to that
  link. It now accepts out of every interface the gateway's routes can use - its egress
  bridge and any link it is routed through - with `masquerade` following onto each. A hop can
  pass its clients' traffic onward into another gateway rather than out to the network, and a
  relay whose every list is a link correctly has no egress bridge at all.

### Fixed

- **The per-app egress bridge is removed with its app.** `Teardown` took the pod down and
  left `zinc-egress-<app>` behind, so one podman network accumulated for every app that ever
  ran with a sibling link plus other networking. The bridge is that app's alone - a bridge
  per app is what keeps apps off each other's L2 - so nothing else can be using it. The link
  networks are still deliberately kept: a sibling may be on one. `ports.NetEnforcer.Teardown`
  returns steps rather than one command's arguments, because removing a pod and removing the
  bridge it used are two things.
- **`zc new --entrypoint`.** Without it `zc new` could not author a runnable app for any
  image whose default command exits immediately, which is most of them - the app started,
  stopped, and said nothing.
- **Compose long syntax imports.** `ports: [{target: 80, published: "8080"}]` and
  `volumes: [{type: bind, source: ..., target: ...}]` aborted the whole import, and that is
  how modern compose files are written. Labels in list form (`["k=v"]`) decode too.

### Security

- **A linked app kept its inbound filter.** While this branch was in progress, an app that
  only *consumed* a sibling (a link list naming another app, nothing published of its own)
  came out with no `input` base chain at all. In nftables a hook with no base chain is not
  filtered - so "no chain" meant unfiltered inbound, not closed, and every other app on that
  shared bridge could reach any port it listened on. An input chain is now emitted whenever
  the app has any link, published or not. Found by review before release; never shipped.
- **The resolver a routed app's DNS is redirected to is now routed through the sibling.** The
  nat rule sent every query to the declared resolver, but nothing put that address on the
  tunnel unless the author happened to include it in a `Via` list's CIDRs - so an app routing
  only some destinations through a VPN sent its DNS out its own egress in the clear, while the
  schema and the renderer both said the queries travel inside the tunnel. The redirect target
  now gets a host route through the sibling.
- **Two configs that read as restrictions and enforced nothing are refused.** `Ports` on a
  routed (`Via`) list, which becomes a blanket accept on the link and would read as "only 443
  through the VPN" while tunnelling every port; and `Interface` on an app that also has a
  sibling link, where the app is on bridges rather than pasta and the port was published on
  every host interface while the config and the authoring warning both named one.
- **An app can no longer resolve into another app's identity.** `AppNameID` is what the runner
  names the container, the pod and the derived image after, and a child that omitted it took
  its base's - so `zcr run notes` would have built, and `zcr stop notes` destroyed, whatever
  `browser` was. A resolved config whose `AppNameID` is not the name it was loaded under is
  now refused.

### Fixed

- `NetworkMeta.DNSServers` reached only linked apps. The ruleset restricts DNS to the declared
  resolvers for *every* filtered app, so one that was restricted and never handed them kept
  podman's resolver and had no working DNS at all - fail-closed, but only because the setting
  half-worked.
- `zc` routed an inheriting VM app to the container runtime: which runner a command goes to is
  decided by `Type`, and `Type` is exactly the kind of field a child takes from its base. The
  delegate now reads the resolved config, like the runners do.
- The `zc` TUI listed an inheriting app by its own file rather than what it resolves to, so an
  app that inherited its image or network showed as blank and "isolated", and actions were
  gated on fields it appeared not to have. The list now shows the resolved app while the edit
  form still opens the file as written.
- The rendered nft ruleset was not byte-identical between runs when both a v4 and a v6 resolver
  were declared - map iteration order leaked into it.
- An IPv6 first resolver on a routed app is rejected at validation instead of failing the whole
  `nft -f -` load with a parse error naming neither the field nor the reason; a `Via` list
  carrying both address families is rejected too, since one list resolves one gateway.
- `ReadyTimeoutSec` is bounded above: large enough values overflowed the runner's nanosecond
  duration to a negative one, turning a very long timeout into no timeout at all.

### Added

- **`NetworkList.Domains`: an egress list can allow a destination by name.** Each name is
  resolved at launch and its addresses join that list's allowed set under the same `Ports`,
  so the ruleset renderer only ever sees addresses and stays pure - the resolution is the one
  part of building a ruleset that can fail, so it happens before anything is rendered and a
  launch whose allowlist could not be resolved does not proceed with a shorter one.
  **The guarantee is narrower than "this app may only talk to these domains", and is
  documented as such rather than left to be assumed.** What is enforced is at the IP layer, on
  the addresses those names held when the app started: an app that resolves somewhere else
  and connects is dropped, an app that connects to one of those addresses *by number* is
  allowed, and nothing inspects a hostname on the wire. Addresses shared by other names are
  shared by this rule. And the snapshot is not refreshed while the app runs, so a domain whose
  addresses rotate drifts out of the set and the app loses access until restarted - stale
  entries stop working rather than quietly allowing whoever holds the address now. A name that
  does not resolve fails the launch and names itself.
  Refused rather than accepted-and-mis-enforced: `Domains` on an ingress list (an incoming
  packet carries an address, not a name, so resolving would admit whoever holds that address),
  on a blacklist (blocking a name would mean blocking every address it is *not* resolved to -
  the rule would read as a ban while stopping only today's addresses), and on a sibling link
  (gated by interface, so an address set on it enforces nothing).
  Measured from inside a running container rather than from the rendered argv: an app allowing
  `example.com:443` reached both of its addresses, was blocked on `1.1.1.1:443` - the same host
  its DNS is allowed to reach on 53 - and blocked on an unrelated address. `ports.NetEnforcer`'s
  `Prepare` now returns an error alongside its steps.

- **`Inherits`: an app can start from another one.** A child names a base and states only
  what differs, taking the rest from it - the duplication across a family of similar apps
  drops to the part that is actually different. Resolution is live: the merge happens on
  every read, so editing a base changes every app built on it at the next launch. That is the
  point, and the cost is that a child's own file no longer tells you what it is, so
  `zc validate <app> --resolved` prints exactly what it merges to.
  **The merge is performed on the YAML rather than on a decoded config, and that is the whole
  design.** Only the text records which keys a config actually *stated*: once decoded,
  `HostTheme: false` is indistinguishable from an absent `HostTheme`, and an empty
  `Capabilities` from an omitted one. Merging structs would have to read every zero value as
  "inherit", which means a child could never turn a base's flag off and never empty a base's
  list - a base could grant a capability or set `DisableSecurityContext` and no child could
  walk it back. Merging nodes has no such rule, so a stated `false` wins and a stated empty
  list wins. Nested blocks merge field by field; a stated list replaces rather than appends,
  because capabilities that only accumulate down a chain are the wrong direction here.
  Fail-closed throughout: a cycle, a missing base, or a chain deeper than 8 fails the read
  rather than yielding a partial config, and `Inherits` is charset-checked before any file is
  opened, since the name is joined into a store path.
  Every store now exposes both reads - `Load` (the file as written) and `LoadResolved` (what
  the app is). Launching, validating and listing take the resolved form; anything that writes
  the config back takes the raw one. For the same reason `zc` **refuses to save an app that
  inherits**: a form knows an app's values but not which of them it stated, so writing them
  all back would replace everything the child inherits with zeros, silently, in a file that
  looks normal afterwards. An inheriting app is edited as a file. Apps that inherit nothing
  are untouched - same load, same save, byte-identical files.

- **`zc compose export` and `zc compose import`: interop with the Compose specification,**
  in both directions and honest about both. Neither is lossless, so neither is silent about
  it - every field that did not cross is printed as a note.
  *Export* describes an app in the format other tools read. Image, entrypoint, user,
  capabilities, resource limits, mounts, published ports, resolvers, `depends_on` and the
  `ReadyCheck` (as a `healthcheck`) all carry. `ReadyTimeoutSec` deliberately does not: it
  bounds how long a *dependent* waits, and compose's `healthcheck.timeout` bounds one probe's
  run, so writing either as the other would export a different promise under the same number. The egress lock-down does not, and cannot: a compose file
  has no way to say "an nftables ruleset is applied to this netns and locked before the
  process starts". The generated file therefore leads with a comment saying it DESCRIBES an
  app rather than sandboxing it, and an app carrying NetworkLists exports with that stated in
  capitals. A VM app is refused rather than half-rendered - a guest is not a container.
  Verified by feeding the output back to the real `podman-compose config`, which parses and
  normalises it cleanly.
  *Import* onboards an existing compose service, and tightens as it goes. Compose cannot say
  what a service may reach, so reading its silence as "full network access" would import a
  posture nobody chose: an imported app arrives with no network at all, and published ports
  are the only exception because they are the only thing stated. The same reading applies
  throughout: a mount that does not say `ro` becomes read-only and noexec (compose's default
  is read-write, and `:z` means read-write too); a port the file bound to loopback is imported
  as sibling-only rather than published to the LAN; `cap_add: ALL` is dropped, and so are
  `NET_ADMIN` and `SYS_ADMIN`, which would let an imported app remove its own network
  lock-down; a numeric `user` is dropped - including the `1000:1000` spelling - since Zinc
  passes the user to podman by name; a relative or `~` host path is dropped rather than
  resolved against the wrong directory; and a mutable tag is pinned to its digest before the
  app is saved. Anything carried that widens the sandbox says so, including every capability. Multi-service files import as one app each; `--service` takes just one and
  `--dry-run` prints instead of saving.
  The polymorphism of real compose files is handled rather than rejected: `depends_on` in
  both its list and map forms, and every list field written as either a scalar or a sequence,
  because `expose: 5432` and `command: nginx -g daemon off;` are how people actually write
  them. That last one was a bug found by running it: taken whole, the command became an
  Entrypoint that is a filename with spaces in it - valid on paper, unexecutable in fact. It
  is now split the way compose itself splits it, reduced to the executable, with the dropped
  arguments named.

- **`StartConditions.ReadyCheck`: `DependsOn` can wait for ready, not just for running.** A
  dependency counted as up the moment its container was, which is true enough for a service
  whose process is its readiness and false for anything a dependent routes through. A VPN
  container is running long before its tunnel is, and a client started in that window comes up
  with a default route and a resolver pointing at a gateway that cannot forward yet - it fails
  closed, but it fails, and nothing says why.
  `ReadyCheck` is a command in exec form (`["sh", "-c", "ip link show wg0 | grep -q UP"]`),
  and `ReadyTimeoutSec` bounds the wait (default 60s). Reused rather than reinvented: the probe
  is installed as the container's own healthcheck, so `podman ps` reports health for exactly
  the command the launch sequence waits on, instead of a readiness notion only the runner
  knows about. It goes in as `CMD-SHELL` with each word quoted, not as the tidier JSON exec
  form: podman 5 parses that and podman 4.9 - what Ubuntu LTS ships, and what CI runs - hands
  the whole bracketed string to a shell instead, so the check could never pass. A
  launch-blocking gate has to work on the podman people actually have; the cost is that the
  image needs a shell.
  A dependency that never becomes ready fails the dependent's launch and names itself, rather
  than starting an app whose every connection will fail. Only a dependency this launch started
  is waited on: one that was already running was gated the same way by whoever started it, and
  re-probing it would turn every launch behind a momentarily-unhealthy dependency into a
  failure rather than closing the start-order race. Apps that set nothing are unaffected - no
  probe, no wait, and the same podman argv as before.

- **`ResourcesMeta` and `InternalUserMeta` are enforced.** Both shipped in the schema at 0.1,
  were range- and charset-validated ever since, and never reached podman: the runtime emitted
  no `--cpus`, `--memory`, `--pids-limit` or `--user` anywhere. So an app could set
  `UseNonRootUser: true` and `MaxRamMiB: 2048`, be told the config was valid, and run as root
  with no memory limit. On a sandboxing tool that is the worst shape a setting can have -
  a security control that reads as if it were in force. What made it an oversight rather than
  a decision is that VM apps already *reject* these fields as container-only, so the schema
  knew they meant something; only the container runtime never looked.
  `MaxSwapMiB` is granted on top of `MaxRamMiB` rather than passed through. podman's
  `--memory-swap` is the total of memory and swap, so forwarding the swap figure alone would
  have capped an app asking for 2048 MiB of RAM and 512 of swap at 512 MiB - the opposite of
  granting it swap. It now requires a memory limit alongside, and the runtime sends the sum.
  Key mounts follow the user: an app running as `app` gets its keys under `/home/app`, not
  `/root`, where they would have been present and unreadable and looked like a broken key.
  Verified against the kernel rather than the argv: a new end-to-end scenario launches an app
  with a 128 MiB cap, 32 MiB of swap, 50 PIDs and `NonRootUserName: nobody`, and the app
  reports back what it was actually granted - `UID=65534`, `memory.max=134217728`,
  `memory.swap.max=33554432`, `pids.max=50`. Emitting a flag is not the same as a limit
  existing: rootless podman needs cgroup v2 delegation for any of it to be real, and a host
  without it would grant nothing and say nothing.

- **DNS for a routed app** - `NetworkMeta.DNSServers`, required once any list sets `Via`.
  Measured first, which changed the fix: a routed app was not leaking DNS, it could not
  resolve at all. podman writes the container's `resolv.conf` and points it at the network's
  own resolver, and on an `--internal` bridge that resolver answers sibling names and forwards
  nothing - an external name comes back NXDOMAIN. Handing the pod a resolver is necessary and
  not sufficient: what settles where a query goes is the netns, not the file.
  So the query is redirected in the app's own netns rather than fought over in a file: DNS to
  any address is rewritten to the declared resolver, which is routed through the sibling like
  any other destination. It travels inside the tunnel and stops with it - verified end to end,
  including that names stop resolving the moment the gateway is stopped. Anything not
  redirected is dropped, so an app carrying a hardcoded resolver cannot step around it.
  Only a routed app is redirected. For an ordinary one the network's resolver works and is the
  only thing that knows its siblings' names, so taking it away would cost something and buy
  nothing. The cost for a routed app is that it resolves through the declared server alone,
  sibling names included; it has already lost those to the NXDOMAIN above.

- **Route an app through a sibling** - `Via: true` on an egress list naming another app.
  Its CIDRs go to that sibling over their private link instead of out the app's own egress,
  which is how an app is put behind a VPN container without trusting it to route itself. Per
  list, so one app can send work subnets through one sibling and everything else direct.
  The guarantees come from the topology rather than from rules being right: the link is an
  `--internal` bridge, so a client whose only interface is that bridge has no other path to
  those destinations and cannot leak past the sibling; and when the sibling stops, its traffic
  blackholes rather than falling back. Both measured end to end through `zcr`.
  The sibling must agree, with `Forward: true` on its own link ingress list - forwarding for
  other apps makes an app a router, so it is never implied by another app naming it. Such an
  app gets `net.ipv4.ip_forward=1` at pod creation (a container cannot set it itself:
  `/proc/sys` is read-only in the namespace, so it would have dropped every packet it was
  meant to forward), a default-drop `forward` chain, and `masquerade` out of its own bridge -
  without which replies would be addressed to a private link address the outside cannot route
  back to.
  No address is ever written into a config. podman assigns the gateway's and it changes when
  the gateway is recreated, so the route is resolved at launch through the network alias
  podman already gives every app on a link. That step runs before the ruleset, because
  resolving needs DNS and the ruleset closes the netns; both still run before the app, so the
  app never sees an unlocked network.

- **A sibling link may coexist with other networking on one app.** It could not before: the
  ruleset was interface-gated or address-gated, never both, and whichever ran ignored the
  other kind of list outright - so a linked app's egress rules simply vanished, which is why
  the combination was rejected at launch rather than mis-enforced. The renderer now gates both
  at once. This is the prerequisite for routing through a sibling: a gateway app has to serve
  its siblings over a private link *and* reach the outside to be worth routing through.
  Chain policy is taken from the non-link lists alone. A link list is structurally a whitelist,
  so counting it would flip an app that pairs a link with an all-blacklist egress to
  default-drop and silently deny everything the blacklist meant to leave open.
  Such an app also cannot use pasta - podman refuses more than one network outside bridge mode
  ("cannot set multiple networks without bridge network mode") - so it gets `zinc-egress-<app>`,
  a routable bridge of its own. Its own, rather than the shared default one: apps on a single
  bridge can reach each other over L2, which would leave isolation resting on the nft rules,
  and an all-blacklist app runs default-accept. Link-only and ordinary apps are untouched.

### Changed

- **`NotificationMeta` is refused instead of ignored.** Nothing in Zinc proxies, silences or
  prefixes an app's notifications, so every field in the block is inert. Accepting `Silenced`
  told an author their app was muted while it notified freely. A non-default value is now a
  validation error naming the reason; the zero value every existing app has stays legal.

## [0.6.0] - 2026-07-27

Windows-class guests. The VM runner now describes a machine rather than assuming one, and
what it cannot do from the host it hands to the guest as something the guest can run.

### Added

- **Windows-class guests.** A guest can now be given the machine Windows requires: UEFI
  firmware with a per-app writable variable store, Secure Boot, an emulated TPM 2.0 backed
  by swtpm, and `Devices: Compatible` - an AHCI disk, an Intel NIC and a USB tablet, which
  every mainstream OS drives out of the box. Windows Setup has drivers for none of the
  virtio hardware and reports finding no drives at all when pointed at a virtio disk, which
  is why the device profile is a field rather than an assumption. `Display: Compatible` adds
  a plain-VGA mode for guests with no virtio-gpu driver.
  A TPM guest needs the current 4 MB OVMF build (`edk2-ovmf` on Fedora 41+, `ovmf` on
  Debian 12+), and Zinc prefers it wherever both generations are installed. The legacy 2 MB
  build does not hand the TPM to the guest, and the failure is close to invisible: qemu
  publishes the TPM's ACPI device itself, so the guest enumerates it and binds a driver to
  it, and only Windows notices there is nothing behind it. It reads TPM version 0 and says
  no more than "This PC doesn't currently meet Windows 11 system requirements". Zinc warns
  when a TPM guest can only get the legacy build. Variable stores are not interchangeable
  between the two generations, so one written by the other build is refused by name rather
  than handed to qemu, which would boot it with a quietly wrong Secure Boot state.
- **Every VM app gets its own machine identity.** qemu with no `-uuid` reports the SMBIOS UUID
  `00000000-0000-0000-0000-000000000000` and gives the NIC the MAC `52:54:00:12:34:56`, both
  shared with every other default qemu VM. Windows Autopilot identifies a device by a hash
  over exactly those fields, so a guest with the defaults can match a stranger's corporate
  enrolment: a fresh Windows 11 install here reached OOBE demanding a sign-in to SAP's tenant,
  branded with their logo. Both are now derived from the app name, so they are unique per app
  and stable across restarts and resets - a changing UUID would make Windows think the
  hardware had been swapped and ask to be reactivated. An install has no app yet and runs
  under a fixed placeholder name, so it seeds its identity from the disk's own path instead:
  deriving from the placeholder would have given every install on every host the same UUID,
  which is the collision this exists to prevent, at the one moment it matters most - OOBE runs
  during an install, and OOBE is what reads it.
- **A fixed guest screen size** - `DisplayWidth`/`DisplayHeight`, `zc new --resolution WxH`,
  `zvr install --resolution WxH`. A guest with no display driver takes whatever mode the
  firmware gave it at boot and cannot change it, so `Display: Compatible` was always exactly
  1280x800 and resizing the window only scaled those pixels. That number comes from plain
  VGA's built-in EDID; the device has no resolution property at all. Asking for a size
  switches the guest to `bochs-display`, which the firmware does honour (measured: Windows
  Setup rendering at a true 1920x1080). `virtio-vga` and `qxl-vga` accept the same properties
  but are not used, because OVMF drives their VGA-compatible half and falls back to 1280x800.
  The one cost is that `bochs-display` has no VGA-compatible mode, so a fixed size requires
  UEFI; validation refuses the pairing rather than letting a BIOS guest boot to a blank
  window, and a guest that asks for nothing keeps plain VGA exactly as before.
  Sizes up to 3840x2400 work, 4K included. Getting there needed two more things, because the
  display's own defaults break down above about 3200x1800 and both failures are silent - the
  guest simply comes up at 1280x800 with nothing logged. Its framebuffer is now sized to the
  screen (the 16 MiB default is less memory than a 4K screen needs), and the generated EDID's
  refresh rate is lowered just enough for the mode to exist: that rate multiplies a pixel
  clock stored in 16 bits, and at QEMU's default 75 Hz a 4K clock overflows the field. The
  rate is close to cosmetic for a guest, whose framebuffer is virtual and whose presentation
  the host compositor drives. Above that, the EDID's active-pixel fields are 12 bits wide, so
  neither side may exceed 4095: 3840x2400 is fine and 4096x2160 is not. Validation refuses
  what cannot work rather than letting it fall back without a word.
- **`MacAddress` / `zc new --mac`** to set the guest NIC's address. The derived default sits
  under QEMU's own `52:54:00` prefix, which says plainly that the machine is a QEMU guest; an
  app that should not announce that can supply its own. A locally-administered address (first
  octet `02`, `06`, `0a` or `0e`) belongs to no vendor and so identifies nothing. The value is
  screened before it reaches qemu, where a comma would start a new device property.
  `--mac random` draws one instead of making you invent it. It is drawn once, at authoring
  time, and the literal address is written into the config: a config that said "random" would
  draw a new address on every run, and a guest whose NIC changes underneath it loses its DHCP
  lease and looks to Windows like swapped hardware.
- **`zvr install`** - runs an OS installer to produce a base disk, for guests that have no
  cloud image. It takes flags rather than an app name deliberately: an app pins its base by
  digest and a disk that does not exist yet has no digest, so requiring one would be a
  chicken and egg. When the install finishes it prints the disk's digest and the `zc new`
  line to author an app against it; from then on every run is an overlay, so `zvr reset`
  returns the app to its freshly installed state.
- `zc new --firmware/--secure-boot/--tpm/--devices` to author all of the above.
- **`InstallMedia` discs are attached on every run**, not only during the install, and
  **`zc new --media`** authors them. An OS is only the first thing a guest wants from a CD:
  the drivers its installer had no room for arrive the same way, and a guest whose network is
  not up yet has no other route in - which is exactly the position a fresh Windows guest is
  in, since the driver that would fix its display is on a disc it could no longer be handed.
  Only the boot order stays the install's own, so an ordinary run boots the disk with the disc
  simply present. The discs are read-only, so leaving one in a config costs a drive letter.
- **`zinc-setup.cmd`, a script the guest can actually run.** Everything Zinc can do for a
  Windows guest stops at the machine: the drivers that make that machine worth having are on
  the virtio-win disc and can only be staged from inside Windows by a user holding an
  administrator token. So `zvr` now writes the guest a script rather than writing the reader
  instructions. It elevates itself (a double-clicked `.cmd` has no token, and without one
  `pnputil` fails on every driver), finds the driver disc by looking for a file only it has
  rather than by a drive letter that depends on what else is attached, falls back from `w11`
  to `w10` folders for an older disc, and stages the display, disk and network drivers. All
  three, not just the display one: switching an app to `Devices: Virtio` without `viostor`
  already staged leaves Windows unable to see its own boot disk. Nothing it does changes the
  running machine, so it is safe to run twice and safe to run early.
  It rides the disc every VM app already gets - the one a Linux guest reads cloud-init from,
  which a `Devices: Compatible` guest has never heard of. That disc is now also built when
  cloud-init is disabled, which a Windows guest would reasonably do, so turning cloud-init
  off no longer takes the script away with it.
- `zc new` now prints the same advisories `zc validate` does. Authoring is when a
  valid-but-surprising choice is cheapest to change; meeting it later as a black screen or an
  open port gives nothing to connect it back to.
- **`make -C virtualization/runner windows-demo WIN_ISO=...`** - the whole Windows flow from
  one argument. The Windows ISO is Microsoft's and cannot be fetched for you; everything
  else is handled, including downloading the virtio-win driver disc (resumable, since it is
  ~700 MB) and defaulting the machine to what Windows 11 requires. When Setup finishes it
  pins the installed disk and authors an app against it.

### Fixed

- **`zc tui` could silently rewrite a Windows guest's display mode.** The form's display row
  offered Accelerated, Window and None but not Compatible, and the enum treats a value it
  does not recognise as "before the first" - so opening a guest that runs on Compatible and
  pressing the cycle key once moved it to Accelerated. That is the pairing that boots to a
  black screen. The row now offers every mode validation accepts, and a test fails if the
  schema gains one the form has not been taught.

### Known limitations

- **`zc tui` cannot author a Windows-class guest.** Its VM form covers the guest's sizing and
  cloud-init identity, not the machine: firmware, Secure Boot, TPM, the device profile, the
  fixed screen size, the MAC and the discs are `zc new` flags or advanced-editor fields. The
  form does preserve all of them when an app authored elsewhere is edited and saved, so this
  costs a round trip rather than a config.
- **Windows guests get no 3D acceleration.** virtio-gpu's Windows driver is display-only -
  there is no virgl path, so guest OpenGL and Vulkan have nothing to run on and the desktop
  is an unaccelerated framebuffer. Passthrough, the usual answer, needs a second GPU. Windows
  guests are for software that must run on Windows, not for games.
- **A guest's screen size is fixed at boot.** Resizing the window scales those pixels rather
  than changing the guest's resolution, because a guest with no display driver cannot be told
  about a new mode. The way off it is a real driver inside the guest: stage `viogpudo` from
  the virtio-win disc, then re-author with `--display Window`. Do it in that order. Measured
  on Windows 11: given a virtio-gpu with no driver staged, the firmware paints and then the
  screen goes black the moment Windows starts, because the device is left with no active
  scanout - qemu's window just reads "display output is not active". Authoring that pairing
  now warns, at `zc new` and at every `zvr run`, rather than letting a black window be the
  first news of it.
- **An installed Windows desktop stops at 1824x1080** without a display driver, whatever the
  machine gives it. Measured on a Windows 11 24H2 guest: at 1920x1080, 1920x1024 and 1920x1200
  it painted 1824 columns and no more than 1080 rows, leaving the rest black; at 1824x1080
  nothing was clipped. Past that it does not simply clip. Asked for 2560x1440 it painted
  810 rows of sheared, repeating bands - and 810 x 2560 is exactly 1920 x 1080, so the desktop
  is writing rows of its own width into a scanout with a wider stride, and every display row
  swallows 1.33 guest rows. Windows Setup is not affected and uses whatever mode it is given,
  so this is the installed desktop's own behaviour. Until a guest display driver is installed,
  author a Windows app at `--resolution 1824x1080`: the largest desktop it will actually
  paint, with nothing wasted.

## [0.5.0] - 2026-07-26

Guest GPU access. A VM app's 3D now reaches the host GPU for both OpenGL and Vulkan.

### Added

- **Guest Vulkan** (`VirtualizationMeta.Vulkan`, `zc new --vulkan`) - passes the guest's
  Vulkan through to the host GPU via qemu's venus. This is the half that matters for games,
  since Proton, DXVK and vkd3d are all Vulkan; without it a guest's Vulkan runs on the CPU.
  Measured on a host with the proprietary NVIDIA driver, rendering real frames: the guest
  reports `Virtio-GPU Venus (NVIDIA GeForce RTX 5080)` and scores **2243** in vkmark, while
  OpenGL reports `virgl (NVIDIA GeForce RTX 5080/PCIe/SSE2)` and scores **3947** in glmark2
  against **931** for the software renderer.
- **`make -C virtualization/runner virgl-venus`** - builds the venus-capable virglrenderer
  that guest Vulkan requires, into the user's own data directory, never touching the system
  copy. Distributions ship virglrenderer built *without* venus, so one has to be built;
  `zvr` finds it automatically and `ZVR_VIRGL_PREFIX` overrides the location.

### Changed

- **Vulkan is opt-in, and says what it costs.** Enabling it disables qemu's seccomp sandbox
  **for that app only**: venus runs in a helper process that virglrenderer forks, and the
  sandbox both forbids the fork and kills the child that inherits its filter - silently,
  surfacing only as a generic "virgl could not be initialized". `zc validate` warns that the
  guest gains GPU Vulkan while the qemu process loses its syscall filter. An app that does
  not ask keeps the sandbox and still gets accelerated OpenGL.
- `zvr` refuses to launch a Vulkan app when no venus-capable virglrenderer is present,
  printing how to build one, rather than starting qemu and letting the guest quietly fall
  back to software rendering.

### Known limitations

- Guest Vulkan and qemu's seccomp sandbox are mutually exclusive; there is no way around it
  today, because the venus helper process cannot survive the filter.
- The venus virglrenderer has to be built locally until distributions ship one.
- Everything else from 0.4 stands: no egress filtering for VM apps, no host directory
  sharing, no snapshots, x86_64 guests only.

## [0.4.0] - 2026-07-26

Adds VM apps. Guests boot, display and are managed; making their 3D actually reach the GPU
is 0.5 work, and the limits below say exactly where that line falls.

### Added

- **`zvr` (zinc-virtualization-runner)** - boots VM apps as qemu guests, the sibling of
  `zcr`. One store and one schema hold both app kinds, split by `Type`, and each runner
  refuses the other's apps by name rather than half-running them. Disks are copy-on-write
  over a digest-pinned base image that is never written to, so `zvr reset` returns an app to
  the image it was authored against. A cloud-init seed gives a fresh guest its hostname,
  user and SSH key. Supervision is a pidfile plus QMP, and the same socket carries the ACPI
  power button, so `zvr stop` lets the guest's own OS flush and unmount rather than being
  killed mid-write. `zvr run --dry-run` prints the exact qemu command line, as `zcr` does for
  podman.
- **VM apps in the schema** - `Type: ZincVirtualization` plus a `VirtualizationMeta` group:
  the guest's sizing, an explicit display mode, user-mode port forwards and cloud-init
  identity. `ImageMeta` is shared rather than duplicated - `Image` is a container reference
  or a base disk path depending on `Type`, and `Install` steps become a derived image's RUN
  layer or the guest's cloud-init `runcmd`. The two kinds **reject each other's fields**
  rather than ignoring them: a VM app carrying `Capabilities` or `NetworkLists` does not
  save, and each message says what supporting it would take. A field that looks configured
  while doing nothing is worse than one that refuses, because the author believes in a
  boundary that is not there.
- **Accelerated guest display** - `Display: Accelerated` attaches `virtio-gpu-gl` and a local
  window, so guest 3D runs on the host GPU and frames reach the compositor without leaving
  the machine or being encoded. This is **OpenGL only**: a guest's Vulkan falls back to
  llvmpipe, software rasterisation on the CPU, which matters because Proton, DXVK and vkd3d
  are all Vulkan. See the known limitations below.
- **`make -C virtualization/runner demo`** - downloads a Fedora Cloud image (verified against
  the digest Fedora publishes), authors a VM app with an accelerated display and boots it, so
  the whole path can be tried in one command.
- **`make -C virtualization/e2e e2e`** - end-to-end tests that boot a real guest under qemu
  and assert what unit tests cannot: that it boots and is reachable, that a base image
  altered after authoring refuses to run, and that a graceful stop is graceful. Not a CI
  gate - GitHub's runners have no `/dev/kvm` - so it is a local pre-release gate that skips
  cleanly when qemu or KVM is missing.

### Changed

- **BREAKING: `zcc` is now `zc` (`zinc-creator`)**, and `container/creator/` moved to
  `creator/`. The creator no longer creates only containers, so the kind letter in its name
  was wrong. There is no separate `zvc`: a VM app is the same YAML with a different `Type`,
  and `zc` already had the form, keybinds, validation and CLI for authoring it, so the
  non-compiling `virtualization/creator/` skeleton was deleted rather than migrated. The
  runners keep their kind letter, because `zcr` and `zvr` really are different things.
  Reinstall the binary under its new name; app files are unaffected.
- **`zc` authors both kinds.** `zc new --vm` writes a VM app, and the form grows a type row
  that rebuilds the rest of itself. Runtime commands route by the app's `Type` - `zcr` for
  containers, `zvr` for guests - with commands that have no counterpart refused by name
  rather than silently doing nothing.
- **VMs are run over qemu directly, not libvirt**, a deliberate departure from the
  architecture doc. libvirtd spawns the qemu process outside the user's session, so it cannot
  open an accelerated window on their compositor, and SPICE plus a viewer adds a hop that
  defeats the point for anything interactive. The cost, accepted knowingly, is that `zvr`
  owns supervision and has no snapshots or managed save.

### Known limitations

- VM apps have **no egress filtering**: the nftables model lives inside a container's own
  network namespace and does not reach a guest, so a VM app gets user-mode networking with
  explicit port forwards, each bound to 127.0.0.1. `NetworkMeta` on a VM app is an error.
- **No host directory sharing** into a guest (that needs virtiofs), no snapshots, x86_64
  guests only, and `zvr console` prints the console socket rather than attaching to it.
- **OpenGL is hardware-accelerated, confirmed by measurement.** A Fedora guest reports
  `OpenGL core profile renderer: virgl (NVIDIA GeForce RTX 5080/PCIe/SSE2)`, GL 4.2 core -
  the guest's GL is running on the host GPU, not in software.
- **Guest Vulkan is NOT accelerated.** It falls back to llvmpipe, which is the CPU, so
  anything using Proton, DXVK or vkd3d renders in software. Vulkan needs qemu's `venus=on`,
  which needs a host `virglrenderer` built with venus support; Fedora 43's is not, and
  enabling venus without one fails the whole renderer and leaves the guest with no display
  at all rather than degrading to OpenGL. Building a venus-capable virglrenderer is not by
  itself sufficient either: venus runs in a separate render-server process, and its
  handshake still fails on this host. Tracked for 0.5.

## [0.3.1] - 2026-07-25

Fixes the two things that made the 0.3 overlay look broken: a frozen window during a launch,
and a grid of thumbnails that never appeared.

### Changed

- **`zlg` no longer freezes while an app starts.** `menu`'s activate callback now runs off the
  Wayland event loop, so the overlay keeps drawing - with a `launching <app>...` banner - while
  `zcr` works, instead of sitting frozen on screen holding an exclusive keyboard grab for the
  whole launch (a second or two normally, minutes when a derived image has to be rebuilt).
  Esc dismisses the window immediately; the launch carries on in the background and `menu.Run`
  returns once it finishes, so no activation outlives the call. A second Enter while one launch
  is in flight is ignored rather than starting a duplicate. Callers name the banner's verb with
  the new `Options.BusyVerb`.
- **Thumbnail and icon decoding is bounded in bytes, not pixels.** The old guard capped declared
  pixels, which is not a memory bound: the same dimensions cost one byte per pixel as a paletted
  image and eight at 16 bits per channel, so a deep-colour file crafted to sit just under the
  pixel cap still decoded to several times what the cap implied. The budget is now the decoded
  size the file's own header implies, which cuts the grid's adversarial worst case from about
  1.7 GiB to 480 MiB while still accepting every plausible wallpaper (an 8K photograph decodes
  to about 132 MiB).

### Fixed

- **Grid thumbnails never appeared unless they decoded within the first 160ms.** The frame
  callback that paces the decode poll is double-buffered Wayland state, applied only when the
  surface commits, and the poll's "nothing landed yet, ask again" path asked without committing.
  The callback therefore never fired and the poll died silently, leaving every tile on its
  placeholder until the next keypress. It went unnoticed because the entrance fade commits a
  frame per refresh while it runs, which is long enough for the small sample images but not for
  a directory of real photographs.

### Known limitations

Unchanged from 0.3.0: the keymap is US-QWERTY, and grid thumbnails decode without
prioritisation or cancellation. The launch freeze is gone, but a launch still cannot be
**cancelled**: Esc dismisses the overlay and the launch runs to completion behind it.

## [0.3.0] - 2026-07-25

Adds the GUI launcher.

### Added

- **`zlg` (zinc-launcher-gui)** - the graphical sibling of `zlt`: the same quick
  picker over the defined apps, as a floating `wlr-layer-shell` overlay. Type to
  filter, up/down (or ctrl+p/n) to move, enter launches the selected app through
  `zcr` and quits, and a dot marks apps already running. `zlg <app>` launches one
  directly, for a desktop hotkey. The picker window itself is the new `menu` module
  (below), so `zlg` is a thin consumer that just supplies the app list and an activate
  callback; it inherits a **static, `CGO_ENABLED=0`, runs-anywhere, byte-reproducible**
  binary with no cgo and no graphics libraries. Like `zcc` and `zlt` it shells out to
  the `zcr` binary and never imports the runtime.
- **`launcher/common`** - a shared library holding the read-side app store, the `zcr`
  delegate, and the fuzzy matcher, so `zlt` and `zlg` share one copy of the
  list / launch / match logic (and its security guards).
- **`menu`** - a standalone, reusable overlay-menu module (repo-root `menu/`, module
  path `github.com/crispuscrew/zinc/menu`) extracted out of `zlg`: a pure-Go Wayland
  `wlr-layer-shell` overlay, a software renderer, a keymap, a theme resolver, and a
  fuzzy-filter picker view-model, all behind one call - `menu.Run(items, activate,
  opts)`. It speaks layer-shell directly through a hand-written binding and reads the
  system light/dark theme from the XDG portal, and it depends on **no** Zinc sibling
  module (Go `replace` directives are not transitive), so `zde` and a future wofi-like
  picker can import it too. The fuzzy matcher is copied in as `menu/internal/match`.
- **App grouping** - a new optional `Group` field on the app config (schema v2, additive)
  files apps into sections. The launcher shows a section header per group when idle and
  flattens to a plain ranked list as soon as you type. The demo apps ship with groups.
- **App icons** - the launcher now draws each app's icon in a left column. It resolves the
  existing `Icon` field (a freedesktop icon name or an absolute image path) by looking the
  name up in the icon-theme directories, decoding and scaling in pure Go. It is raster-only
  (PNG/JPEG/GIF); an SVG-only or missing icon just leaves that row's slot blank.
- **Font** - text is antialiased (rendered with `x/image/opentype`), not the old bitmap face.
  It auto-detects an installed system Nerd Font (monospace, so it matches your terminal),
  falling back to the bundled Go Mono when none is found. `ZLG_FONT=/path/to/font.ttf` pins a
  specific font.
- **`menu` grid layout** - the `menu` module can lay items out as a thumbnail grid
  (`Options.Grid`) instead of the default list: each item's new `Preview` image is drawn as a
  tile with its label beneath, arrow keys navigate in two dimensions, and typing still
  fuzzy-filters. Thumbnails decode in the background (bounded, cgo-free, aspect-preserving) and
  appear as they land, so a directory of large images stays responsive. The decode/scale core
  is shared with the icon path in the new `internal/imgutil`, and WebP is now decodable too.
- **`wallpaper` example** - a thumbnail wallpaper chooser built on the grid layout
  (`menu/example/wallpaper`), alongside the `dmenu` example. It scans a directory, shows the
  images as a filterable grid, and on Enter sets the chosen one via `$WALLPAPER_CMD`
  (`swww`/`swaybg`/`hyprpaper`) or prints the path when that is unset - proving the grid is
  reusable by an ordinary program and staying compositor-agnostic.
- **`make -C menu wallpaper-demo`** - the grid's counterpart to the launchers' `demo` targets:
  generates sample wallpapers, builds the example, and opens the grid over them, so the layout
  can be tried in one command. `WALLPAPER_DIR=...` points it at your own images instead. The
  samples are generated deterministically (`menu/example/wallpaper/gen`) rather than committed,
  so the repository carries no binary image assets.
- **Screenshots** - the READMEs now show the launcher and the grid (`docs/media`), captured
  headlessly under sway so they are reproducible and contain nobody's desktop.

### Fixed

- **Double launch in `zlt`** - a second enter while a launch was still in flight started a
  second `zcr run` of the same app. The two raced on one pod, and the loser's fail-closed
  teardown removed the pod the winner had just created, killing the app while the picker
  still reported success. Enter is now ignored until the launch resolves.
- **App names containing `..` were unusable** - the store rejected any name with `..` as a
  substring, not just the traversal segment, so an app called `my..app` was listed by both
  launchers but could never be loaded or launched. The guard now checks path segments; the
  separator check that actually confines a name to the apps directory is unchanged.
- **App names ending in `.yaml` ran the wrong file** - `zcr` reads such an argument as a
  filesystem path, so launching the app `notes.yaml` ran whatever `./notes.yaml` happened to
  be in the launcher's working directory (for a hotkey, `$HOME`) instead of the stored app.
  The launchers now reject that name and point at the explicit `./notes.yaml` path form.

### Known limitations

- `zlg`'s keymap is US-QWERTY; full keyboard-layout (xkb) support is future work.
- Like `zlt`, `zlg` lists and launches; managing an app (stop, logs, edit) stays in
  `zcc`.
- **`zlg` freezes while an app starts.** `menu`'s activate callback is synchronous, so `zlg`
  runs `zcr run` on the Wayland event loop: the overlay stops redrawing and keeps its
  exclusive keyboard grab until `zcr` returns. That is a second or two for a normal launch,
  but minutes for an app whose derived image has to be rebuilt. An asynchronous activate is
  0.4 work.
- The grid decodes thumbnails for every cell it has drawn, with no prioritisation or
  cancellation, so scrolling fast through a very large directory makes the visible tiles
  queue behind ones already scrolled past.
- The thumbnail decode guard caps declared pixels, which bounds a plausible image but not a
  crafted one: a deep-colour file at the pixel limit still decodes to far more memory than
  the cap implies.

## [0.2.1] - 2026-07-22

Adds a runtime volume flag to `zcr`.

### Added

- **`zcr run --volume` / `-v`** - mount an extra host directory or file into an app
  for one run only, without editing its config:
  `zcr run <app> -v HOST:CONTAINER[:OPTIONS]` (repeatable). `OPTIONS` defaults to
  `ro,noexec`; `rw` makes it writable and `exec` executable. The runtime mount is
  appended to the loaded config in memory (never written back to the YAML) and passes
  through the same field-shift / injection validation and the same arg-builder as a
  configured `Volumes` entry. This delivers the "can also be added temporarily at
  runtime" behavior the `Volumes` schema field always described.

## [0.2.0] - 2026-07-19

Adds the launcher.

### Added

- **`zlt` (zinc-launcher-tui)** - a fast, keyboard-first fuzzy picker over the
  defined apps. Type to filter (an in-house subsequence matcher that favours
  matches at the start of a name and at word boundaries), up/down (or ctrl+p/n) to
  move, enter launches the selected app through `zcr` and quits (dmenu-style), and
  a `●` marks apps already running (from `zcr ps`). `zlt <app>` launches one
  directly, for a desktop hotkey. Like `zcc` it depends only on `common` and
  shells out to the `zcr` binary, so it never imports the runtime - dependency
  auto-start, the network lock-down, and derived-image builds stay `zcr`'s job. It
  lives at `launcher/tui`, with `launcher/gui` reserved for the planned `zlg`.

### Known limitations

- `zlt` lists and launches; managing an app (stop, logs, edit) stays in `zcc`. The
  GUI launcher (`zlg`) and richer rows are on the roadmap.
- `virtualization/creator/` is still a non-compiling skeleton (0.7), excluded from
  the build and CI.

## [0.1.0] - 2026-07-16

First release. Ships the container tools: author an app once, run it sandboxed.

### Added

- **`common`** - the shared library: schema version 2 (a flat `AppConfig` with
  grouped `*Meta` structs) and pure validation, run identically at author time
  and at launch time. Third-party images must be digest-pinned.
- **`zcc` (zinc-container-creator)** - a keyboard-first Bubbletea TUI and a
  scriptable CLI to create, validate, edit, rename, and delete app files, with
  selectable keybind schemes (`default`, `vim`, or a custom one) and
  image search / digest-resolve. It depends only on `common` and shells out to
  the `zcr` binary to run apps.
- **`zcr` (zinc-container-runner)** - the rootless-podman runtime:
  `run` / `build` / `validate` / `stop` / `restart` / `inspect` / `logs` /
  `term` / `ps` / `image`. Derived images from `ImageMeta.Install`, terminal and
  multiterminal apps, dependency auto-start, and a dry run that prints the exact
  podman commands and nft ruleset.
- **Fail-closed network lock-down** - applied by nftables in the app's own
  network namespace before the app process starts, with no unfiltered window:
  isolated (localhost only), egress whitelist, LAN publish filtered by source,
  and per-port sibling links over a private internal bridge. A small
  digest-pinned netfilter helper image applies the ruleset.
- **Reproducible, podman-only builds** - a digest-pinned Go toolchain, vendored
  deps, and byte-identical output (`make repro`); a `version` stamped from
  `git describe`; a Go end-to-end suite against real podman; and CI.

### Known limitations

- The network model **rejects at launch** (fail-closed, never mis-enforced):
  host-scoped egress, gateway / multi-homing, and combining a sibling link with
  any other networking on one app.
- Schema-defined but not yet wired into the launch: `ResourcesMeta`,
  `InternalUserMeta`, `NotificationMeta`, and bundle-relative `Configs` mounts.
- `launcher/` and `virtualization/creator/` are skeletons that do not compile
  yet; they are on the roadmap and excluded from the build and CI.

[0.8.2]: https://github.com/crispuscrew/zinc/releases/tag/v0.8.2
[0.8.1]: https://github.com/crispuscrew/zinc/releases/tag/v0.8.1
[0.8.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.8.0
[0.7.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.7.0
[0.6.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.6.0
[0.5.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.5.0
[0.4.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.4.0
[0.3.1]: https://github.com/crispuscrew/zinc/releases/tag/v0.3.1
[0.3.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.3.0
[0.2.1]: https://github.com/crispuscrew/zinc/releases/tag/v0.2.1
[0.2.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.2.0
[0.1.0]: https://github.com/crispuscrew/zinc/releases/tag/v0.1.0
