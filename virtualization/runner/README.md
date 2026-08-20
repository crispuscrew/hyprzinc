# zvr - the Zinc virtualization runner

`zvr` boots VM apps as qemu guests. It is the sibling of `zcr`: one app store, one schema,
split by `Type`, with each runner taking the apps it owns and refusing the other's by name.

```sh
zvr run <app>            boot a guest (detached; the window, if any, is qemu's)
zvr run <app> --dry-run  print the exact command line and ruleset, change nothing
zvr stop <app> [--force] shut it down (ACPI power button, or signal the process)
zvr ps                   running guests
zvr status <app>         one guest's state
zvr validate <app>       check a config, run nothing
zvr reset <app>          delete the guest's disk, back to the pinned base
zvr pin <image.qcow2>    print the sha256 pin to put in a config
zvr net <app> [--json]   what the guest's egress ruleset has seen
zvr console <app>        where to attach for the serial console
```

Apps are authored with `zc` (`zc new <name> --vm ...`, or the `tui` form), never here: a
runner that could rewrite a config could quietly change what it is about to run.

## Why qemu directly, and not libvirt

libvirtd spawns the qemu process, so that process is not in your session and cannot open a
window on your compositor. libvirt's answer is SPICE plus a separate viewer, which puts a hop
between the guest's frames and the screen - and for a VM that exists to run something
interactive, that hop is the whole problem.

`zvr` starts qemu as a child of your session instead, so a guest gets a local,
GPU-accelerated window whose frames never leave the machine. It also matches the container
side exactly: build an argv from validated config, exec a binary, and print it with
`--dry-run` before anything runs.

The cost is paid knowingly: **zvr owns supervision.** Starting, finding and stopping guests
is its job, and there are no snapshots or managed save, both of which libvirt would have
provided.

## What a guest gets

Exactly what its config asked for. qemu is started with `-nodefaults`, so no device arrives
merely because it was compiled in, and the host process runs inside qemu's own seccomp jail
(`-sandbox on`, denying privilege elevation, helper spawning and scheduling changes).

**Disks are copy-on-write.** The base image is never opened for writing; each app has its own
overlay backed by it. `zvr reset` deletes that overlay and the app is back to its authored
image - the VM reading of a disposable container.

**The base is pinned.** `VirtualizationMeta.BaseDigest` is the sha256 of the file's bytes,
because a file digest cannot ride inside a path the way a container digest rides inside a
reference. `zvr pin` prints it. The image is hashed in full on first use and then whenever
its identity moves - device, inode, size and both timestamps at nanosecond resolution - which
catches a base that was replaced, rebuilt or restored. It is not a defence against someone
who can already write to your image directory, since they can rewrite the cache beside it.

**Stopping is graceful by default.** `zvr stop` presses the guest's ACPI power button over
QMP and waits, so the guest's own OS flushes and unmounts; `--force` signals the process
instead, for a guest that has stopped answering.

## Display

`VirtualizationMeta.Display` is explicit, never inferred - whether a guest gets an
accelerated window is the difference between a playable game and a slideshow:

| Value | What it does |
| --- | --- |
| `Accelerated` | `virtio-gpu-gl` plus a local window: guest 3D runs on the host GPU and reaches the compositor as a dmabuf |
| `Window` | a local window with no 3D acceleration |
| `None` | headless; the way in is the serial console |

Accelerated 3D needs a guest with the virtio-gpu driver, which in practice means Linux.

### Vulkan (`VirtualizationMeta.Vulkan`)

`Accelerated` gives the guest **OpenGL** through virgl out of the box. Guest **Vulkan** is
opt-in with `Vulkan: true`, and it matters because Proton, DXVK and vkd3d are all Vulkan -
without it a game in a guest renders on the CPU however good the host GPU is.

It costs two things, both stated rather than hidden:

- **A venus-capable virglrenderer.** Distributions ship virglrenderer built *without* venus
  (Fedora 43's has no venus symbols at all), so one has to be built. `zvr` prints the recipe
  if it cannot find one, and `ZVR_VIRGL_PREFIX` points at an existing build:

  ```sh
  make -C virtualization/runner virgl-venus
  ```

  It installs into your own data directory and never touches the system copy. `zvr` finds it
  there automatically; `ZVR_VIRGL_PREFIX` points at a different build.

- **qemu's seccomp sandbox, for that app.** venus runs in a helper process that
  virglrenderer forks, and the sandbox both forbids the fork and kills the child that
  inherits its filter - silently, surfacing only as a generic "virgl could not be
  initialized". So an app with `Vulkan: true` runs qemu unsandboxed, and `zc validate` warns
  about it. The guest gains GPU Vulkan; the host process loses its syscall filter. That is
  the caller's trade to make, which is why it is off by default.

Measured on a host with the proprietary NVIDIA driver, rendering real frames rather than
just enumerating a device:

| | Renderer the guest reports | Benchmark |
| --- | --- | --- |
| Vulkan | `Virtio-GPU Venus (NVIDIA GeForce RTX 5080)` | vkmark **2243** (2268 fps) |
| OpenGL | `virgl (NVIDIA GeForce RTX 5080/PCIe/SSE2)` | glmark2 **3947** |

The control that makes those numbers mean something: the same OpenGL benchmark against a
software renderer scores **931**, so the accelerated path is about 4x faster on this
workload and far more on anything heavier.

Neither `max_hostmem` nor a shared `memory-backend-memfd` turned out to be necessary; the
working set is `venus=on,blob=on,hostmem=8G` plus the two points above.

## Try it

```sh
make -C virtualization/runner demo
```

Downloads a Fedora Cloud image once (~600 MB), verifies it against the digest Fedora
publishes - refusing to boot if it does not match - authors a demo VM app with an
accelerated display, and starts it. A qemu window opens on your compositor. To check the
display path is really accelerated rather than falling back to software:

```sh
ssh -p 2222 -o StrictHostKeyChecking=no zinc@127.0.0.1 glxinfo -B
```

Look for `virgl` or `virtio_gpu` in the renderer line; `llvmpipe` means the guest fell back
to software rendering.

## Build

```sh
make build        # reproducible static build -> ./bin/zvr
make check        # gofmt + vet + test in the pinned container
make repro        # prove the build is byte-identical
```

Running guests needs `qemu-system-x86_64`, `qemu-img` and `xorriso` on the host, plus
`/dev/kvm`.

### The driver ISO is optional

`windows-demo` fetches the virtio-win driver ISO (~790 MB), but **a Windows install does not
need it**: Setup boots from the Windows media and installs onto the AHCI disk that
`Devices: Compatible` provides. It matters afterwards: switching the installed guest to a
virtio disk and network is a worthwhile speed win, and its display driver is what lifts the
guest off a fixed 1824x1080 framebuffer. Both can be done any time - see below.

So a slow or failed download never blocks an install - the target says so and carries on
with just the Windows media.

### Giving an installed guest its drivers

An installed Windows guest with no display driver paints at most 1824x1080 and cannot be told
about a new mode, so its window scales rather than resizes. The drivers that fix that (and
that make the disk and network fast) are on the virtio-win disc and can only be staged from
inside Windows, by a user holding an administrator token. So Zinc writes the guest a script
instead of writing the reader instructions.

Attach the driver disc:

```sh
zc new mywin --vm ... --display Compatible --resolution 1824x1080 \
    --media ~/.local/share/zinc/images/virtio-win.iso
zvr run mywin
```

Inside Windows, open the small CD drive (volume label `cidata`) and run **`zinc-setup.cmd`**.
It elevates itself, finds the driver disc by its contents rather than by a drive letter that
depends on what else is attached, and stages the display, disk and network drivers with
`pnputil`. Nothing it does changes the running machine, so it is safe to run twice.

That script is generated per app by `zvr` and lands on the same disc a Linux guest gets its
cloud-init from - a guest on `Devices: Compatible` has never heard of cloud-init, so the disc
carries what that guest can actually read.

Then shut the guest down and re-author it:

- `--display Window` and no `--resolution` - the machine gets a `virtio-gpu-pci`
  (`PCI\VEN_1AF4&DEV_1050`, the ID `viogpudo.inf` claims), Windows binds the staged driver,
  and the desktop becomes a real driven display that resizes with the window.
- `--devices Virtio` - a virtio disk and NIC. Only after `viostor` is staged: without it
  Windows cannot see its own boot disk and stops with `INACCESSIBLE_BOOT_DEVICE`.

Re-authoring in the other order is what the launch-time warning about a virtio display on a
compatible guest is there to catch.

`make check-virtio-win` verifies the ISO on disk, and it does more than check that the file
opens. An interrupted-and-resumed transfer can corrupt the middle of the image while leaving
its ISO metadata perfectly readable: the volume mounts, the directory listing looks right,
and a driver inside is silently garbage. The check therefore extracts two drivers Windows
would actually load and confirms they are real Windows binaries. `windows-demo` runs it
automatically and drops a damaged image rather than attaching it.
