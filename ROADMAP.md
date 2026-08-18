# Zinc - Roadmap

Source of truth: [`docs/architecture.md`](docs/architecture.md). Release plan:
[`RELEASES.md`](RELEASES.md). Priority order: **Stable, then Secure, then Beautiful**.

**Style:** functional core / imperative shell. The schema, validation, and the podman
argv/ruleset builders are pure functions over decoded data; I/O and process execution live
at the edges (the adapters, the CLI/TUI). Every release ships with tests and a runnable
exit check.

Legend: done, in progress, planned.

---

## 0.1 - Containers (zc + zcr) - done

The two container tools reach MVP.

Delivered:
- The v2 app-config **schema** and pure **validation** in `common/` (including the rule
  that third-party images must be digest-pinned).
- A YAML config store under `~/.config/zinc/apps`, with save-time and launch-time
  validation.
- **zcr**: real rootless-container lifecycle (`run`/`stop`/`restart`/`inspect`/`logs`/
  `ps`), derived images (`FROM image` + the install layer), multiterminal apps, and a
  `--version` stamped from git.
- **zc**: a keyboard-first Bubbletea TUI plus a scriptable CLI; it authors app files and
  shells out to `zcr` to run them.
- **Network lock-down**, applied in the app's own netns by an nftables init step before the
  app starts, with no unfiltered window: isolated (localhost only), egress whitelist,
  ingress publish to the LAN, and per-port sibling links over a private bridge.
- Podman-only reproducible builds, an end-to-end suite against real podman, and CI.

Known gaps (honest, tracked): the network model still rejects (does not run) host-scoped
egress and gateway / multi-homing; bundle-relative config mounts are deferred. (Combining a
sibling link with other networking on one app was lifted after 0.6 - it is what routing an app
through a VPN sibling is built on.) Test coverage is partial away from the
security path. (At 0.1 `launcher/` and `virtualization/creator/` did not compile either - they
still referenced the removed `core` module. `launcher/` was rebuilt in 0.2 and 0.3; the
virtualization skeleton was deleted in 0.4, when `zc` took over authoring both app types.)

## 0.2 - Launcher TUI (zlt) - done

A fast, keyboard-driven picker (TUI) over the defined apps: fuzzy filter as you type,
enter launches the selected app through `zcr` (which handles dependency auto-start), a
running indicator from `zcr ps`, and a `zlt <app>` direct-launch form for a desktop
hotkey. Like `zc` it depends only on `common` and shells out to the `zcr` binary, so it
never imports the runtime. Lives at `launcher/tui`, leaving `launcher/gui` for `zlg`.

## 0.3 - Launcher GUI (zlg) - done

A graphical sibling to `zlt`: the same quick picker over the defined apps, for a
point-and-click launch. Like the other tools it depends only on `common` and shells out to
the `zcr` binary, so it never imports the runtime.

Delivered:
- **zlg** at `launcher/gui`: a floating `wlr-layer-shell` overlay picker - fuzzy filter as you
  type, a dot for apps already running, `zlg <app>` for a hotkey - as a static, `CGO_ENABLED=0`,
  byte-reproducible binary with no cgo and no graphics libraries.
- **menu** (`menu/`, its own module): the overlay core extracted so `zde` and a future
  wofi-like tool can import it - a hand-written layer-shell binding, a software renderer, a
  keymap, a theme resolver and a picker view-model behind one `menu.Run` call, depending on no
  sibling module. Its `ActivateFunc` runs off the event loop (0.3.1), so a slow launch leaves
  the overlay responsive instead of freezing it with the keyboard grabbed.
- **launcher/common**: the read-side app store, the `zcr` delegate and the fuzzy matcher, so
  `zlt` and `zlg` share one copy of the list / launch / match logic and its security guards.
- **Richer rows**: app grouping (schema v2's additive `Group` field), freedesktop icon lookup,
  antialiased text in an auto-detected system Nerd Font, and a thumbnail **grid** layout with
  bounded background decoding, shown off by the `wallpaper` example.

Known gaps: the keymap is a fixed US-QWERTY fallback rather than a real xkb interpreter; a
launch cannot be cancelled once started (Esc dismisses the overlay, the launch continues); and
grid thumbnails are decoded without prioritisation, so fast scrolling queues visible tiles
behind ones already scrolled past.

## 0.4 - Virtualization (zvr) - done

VM apps as the container tools' sibling, sharing the same config library and format: one
store holds both kinds, split by `Type`, and each runner refuses the other's apps by name.

Delivered:
- **zvr** at `virtualization/runner`: boots VM apps as qemu guests. Copy-on-write overlays
  over a digest-pinned base (`zvr reset` returns an app to its authored image), a cloud-init
  seed for first-boot identity, supervision by pidfile and QMP, and a graceful stop that
  presses the guest's ACPI power button rather than killing it mid-write.
- **Schema**: `Type: ZincVirtualization` plus a `VirtualizationMeta` group. The two kinds
  reject each other's fields rather than ignoring them.
- **zcc renamed to zc** (`zinc-creator`), moved to `creator/`, and taught to author both
  kinds. There is no separate `zvc`; the old `virtualization/creator` skeleton was deleted.

Deliberate departure from the original plan: **qemu is driven directly, not through
libvirt.** libvirtd spawns the qemu process outside the user's session, so it cannot open an
accelerated window on their compositor, and SPICE-plus-viewer adds a hop that defeats the
point for anything interactive. The cost, accepted knowingly, is that zvr owns supervision
and has no snapshots or managed save. See the architecture doc, Virtualization.

Also delivered: an end-to-end suite against real qemu (`virtualization/e2e`, a local gate
rather than a CI one - GitHub's runners have no `/dev/kvm`), and `make -C
virtualization/runner demo`, which fetches a Fedora Cloud image, verifies it against
Fedora's own published digest and boots it.

**GPU status, measured rather than assumed.** Guest **OpenGL is hardware-accelerated**: a
Fedora guest reports `virgl (NVIDIA GeForce RTX 5080/PCIe/SSE2)` at GL 4.2 core, so guest GL
really is running on the host GPU. Guest **Vulkan is not** - it measures as `llvmpipe`, the
CPU. 0.4 therefore ships as a genuinely OpenGL-accelerated VM runner with software Vulkan;
closing the Vulkan gap is 0.5 (below).

## 0.5 - Guest GPU access - done

Make a guest's 3D actually reach the GPU, which is what turns a VM app from an isolation
tool into somewhere a real workload can run.

Delivered: a VM app's 3D reaches the host GPU for **both** APIs, measured by rendering real
frames rather than by enumerating a device.

| | Renderer the guest reports | Benchmark |
| --- | --- | --- |
| Vulkan | `Virtio-GPU Venus (NVIDIA GeForce RTX 5080)` | vkmark **2243** |
| OpenGL | `virgl (NVIDIA GeForce RTX 5080/PCIe/SSE2)` | glmark2 **3947** (software: 931) |

Getting there meant clearing five blockers, each hiding the next, and the fourth is the one
worth remembering:

1. Distributions ship virglrenderer built **without venus** - Fedora 43's has no venus
   symbols at all. `make -C virtualization/runner virgl-venus` now builds one.
2. venus does not run in qemu's process. It runs in a forked `virgl_render_server` whose
   path is compiled into the library, so a venus-capable library still execs the distro's
   venus-less server unless `RENDER_SERVER_EXEC_PATH` says otherwise.
3. qemu's `-sandbox spawn=deny` forbade that fork.
4. **The rest of the seccomp filter killed the forked child anyway** - inherited, silent,
   surfacing only as a generic "virgl could not be initialized". So the whole sandbox has to
   go, not just `spawn=deny`. That is a real trade rather than a bug to paper over, which is
   why Vulkan is opt-in per app and `zc validate` warns what it costs.
5. The NVIDIA driver was innocent, contrary to the leading theory: a direct Vulkan probe
   showed the 5080 exposing every extension venus needs.

Measured minimal set, so nothing is cargo cult: `venus=on,blob=on,hostmem=8G`. Neither
`max_hostmem` nor a shared `memory-backend-memfd` was required.

Honest boundary on this hardware: full GPU passthrough is not an option here at all. A
single RTX 5080 with no integrated GPU means passing it through blinds the host.

## 0.6 - Windows-class guests - done

Run a guest whose OS has never heard of virtio and refuses to install without hardware the
previous releases did not model. The point is not Windows for its own sake: it is that the VM
runner now describes a machine rather than assuming one.

Delivered:
- **A machine Windows accepts**: UEFI with a per-app writable variable store, Secure Boot,
  an emulated TPM 2.0 over swtpm, and `Devices: Compatible` (AHCI, e1000e, USB tablet) for a
  guest whose installer has no virtio drivers at all.
- **`zvr install`**: runs an OS installer to produce a base disk, for guests with no cloud
  image, then prints the digest and the `zc new` line to author an app against it.
- **A machine identity per app**, because qemu's defaults are shared by every default guest
  in the world and Windows Autopilot identifies a device by a hash over them.
- **A fixed guest screen size** up to 3840x2400, for a guest with no display driver to
  resize itself.
- **`make -C virtualization/runner windows-demo WIN_ISO=...`**: the whole flow from one
  argument.
- **`zinc-setup.cmd`**: a generated script on the guest's own provisioning disc that stages
  the virtio drivers from the virtio-win disc. What the host cannot reach into the guest to
  do, the guest is handed as something it can run.

Three failures worth remembering, because all three are silent and none names its own cause:

1. **The legacy 2 MB OVMF build does not hand a TPM to the guest.** qemu publishes the TPM's
   ACPI device itself, so the guest enumerates it and binds a driver to it; only Windows
   notices nothing is behind it, and all it reports is that the PC does not meet its
   requirements. The 4 MB build works. A Linux guest cannot catch this: its driver probes the
   hardware directly and sees a working TPM on both.
2. **Secure Boot without SMM runs but does not enforce**, so the guest reports it switched
   off while the host shows it enabled.
3. **A guest display falls back to 1280x800 without a word** when any of three separate
   limits is exceeded: its framebuffer memory, the EDID pixel clock's 16-bit field, or the
   12-bit active-pixel fields. Each was measured against real firmware rather than assumed,
   and a size that cannot work is now refused at validation.

Known limits: no 3D for Windows guests (virtio-gpu's Windows driver is display-only, and
passthrough needs a second GPU); the screen size is fixed at boot because a driverless guest
cannot be told about a new mode; and a driverless Windows desktop paints at most 1824x1080
whatever the machine offers it, so a Windows app is authored at that size. The way off the
fixed size is the guest's own display driver: `InstallMedia` discs are now attached on every
run, not only the install, so an installed guest can still be handed the virtio-win disc,
stage `viogpudo` from it and be re-authored with `--display Window`.

**ZDE** (the Zinc Desktop Environment, `zde-niri` / `zde-hypr`) is a separate project in
its own repository, layered on these tools; its milestones are tracked there, not in this
plan. This repo ships only the Zinc core and its tools.

---

## Beyond the version line - container hardening

Container work that matures alongside the releases above, not tied to one version:

- **filtered session bus:** DELIVERED in 0.8. An app gets no D-Bus session bus unless `DBusMeta`
  names one, and then only those names, served by an `xdg-dbus-proxy` container that holds the
  real socket. The proxy is not in the app's pod, so the app cannot signal the process filtering
  it. Authored from both `zc new` and the TUI.
- **vpn-container routing:** DELIVERED in 0.7. An app is routed through a sibling VPN app with
  per-destination backend selection (`Via` per list) and fail-closed DNS; the "combining a
  link with other networking" restriction was lifted to make it possible. What is still open
  is a live rather than launch-time domain allowlist (see `NetworkList.Domains`, 6.2).
- **Trusted image layering:** curated, digest-pinned base images built locally, so an app
  can reference a known-good base without a hand-written Containerfile.
- **Theme bundle, audio, keys, mounts:** a read-only theme bundle + env for host-matching
  GTK/Qt apps; audio granted per direction, by session device or exact ALSA nodes; ssh/gpg key mounts with
  agent sockets and 0600 enforcement; general host mounts.
- **Nix home-manager module + flake:** the tools on `$PATH`, a first-run seed of app files,
  and desktop wiring, all reproducible.
- **Profiles, hotkeys, autostart:** named session profiles, desktop hotkeys, and login
  autostart.

---

### Cross-cutting

- **Honesty:** where a mechanism is partial, say so in the UI and the docs.
- **Every change:** `make check` green (gofmt + vet + test) in every module you touched.
- Known tradeoffs are tracked in the architecture doc.
