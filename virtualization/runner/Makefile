# Zinc Virtualization Runner (zvr). Build logic is shared with the other tools via
# ../../tool.mk. Run from this directory: `make <target>`.

TOOL := zvr

include ../../tool.mk

## demo: download a Fedora Cloud image, author a VM app, and boot it with an accelerated window
# The counterpart to the launchers' `demo` targets. Unlike those it needs a real guest OS, so
# it downloads one once (~600 MB) into the shared image directory and pins it by the digest
# Fedora publishes - if the file does not hash to that, the demo stops rather than booting
# something unverified.
FEDORA_IMAGE   := Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2
FEDORA_URL     := https://download.fedoraproject.org/pub/fedora/linux/releases/43/Cloud/x86_64/images/$(FEDORA_IMAGE)
FEDORA_DIGEST  := sha256:846574c8a97cd2d8dc1f231062d73107cc85cbbbda56335e264a46e3a6c8ab2f

IMAGE_DIR ?= $(HOME)/.local/share/zinc/images
DEMO_APP  ?= zinc-demo-vm
DEMO_PORT ?= 2222
# A console password for the demo guest only. The window shows a login prompt and ssh keys
# do not help there, so without one the qemu window - the whole point of the demo - cannot
# be used. The guest is reachable on loopback only.
DEMO_PASSWORD ?= zinc
SSH_KEY   ?= $(firstword $(wildcard $(HOME)/.ssh/id_ed25519.pub $(HOME)/.ssh/id_rsa.pub))
ZC        ?= ../../creator/bin/zc

demo: build
	@test -n "$(SSH_KEY)" || { echo "no ssh public key in ~/.ssh; make one with: ssh-keygen -t ed25519"; exit 1; }
	@test -x "$(ZC)" || $(MAKE) -C ../../creator build
	@mkdir -p "$(IMAGE_DIR)"
	@if [ ! -f "$(IMAGE_DIR)/$(FEDORA_IMAGE)" ]; then \
		echo "downloading $(FEDORA_IMAGE) (~600 MB, once)"; \
		curl -fSL --progress-bar -o "$(IMAGE_DIR)/$(FEDORA_IMAGE).part" "$(FEDORA_URL)"; \
		mv "$(IMAGE_DIR)/$(FEDORA_IMAGE).part" "$(IMAGE_DIR)/$(FEDORA_IMAGE)"; \
	fi
	@got="$$(./bin/zvr pin "$(IMAGE_DIR)/$(FEDORA_IMAGE)")"; \
	if [ "$$got" != "$(FEDORA_DIGEST)" ]; then \
		echo "the downloaded image does not match the digest Fedora published:"; \
		echo "  published: $(FEDORA_DIGEST)"; echo "  on disk:   $$got"; exit 1; \
	fi; \
	echo "base image verified against Fedora's published digest"
	@./bin/zvr stop $(DEMO_APP) --force >/dev/null 2>&1 || true
	@$(ZC) delete $(DEMO_APP) >/dev/null 2>&1 || true
	@# A fresh disk every time. cloud-init keys its "already provisioned" decision on the
	@# instance id, so re-authoring alone would leave an existing guest un-reprovisioned and
	@# the setup steps below would silently never run.
	@./bin/zvr reset $(DEMO_APP) >/dev/null 2>&1 || true
	@$(ZC) new $(DEMO_APP) --vm \
		--image "$(IMAGE_DIR)/$(FEDORA_IMAGE)" \
		--base-digest "$(FEDORA_DIGEST)" \
		--memory 4096 --vcpus 4 --disk 20 \
		--display Accelerated --vulkan \
		--ci-user zinc --ci-ssh-key "$(SSH_KEY)" \
		--forward $(DEMO_PORT):22 \
		--install 'dnf install -y -q mesa-demos glx-utils vulkan-tools mesa-vulkan-drivers glmark2 weston; echo zinc:$(DEMO_PASSWORD) | chpasswd' \
		--desc "0.4 demo guest: accelerated display smoke test"
	@./bin/zvr run $(DEMO_APP)
	@echo
	@echo "A qemu window should now be open on your compositor, showing the guest boot."
	@echo "First boot installs the 3D test tools, so give it a minute or two."
	@echo
	@echo "Log in at that window as  zinc  /  $(DEMO_PASSWORD)  (demo only, and the guest is"
	@echo "reachable on loopback only). Over ssh you need no password:"
	@echo "  ssh -p $(DEMO_PORT) zinc@127.0.0.1"
	@echo
	@echo "Guest Vulkan is on, which needs a venus-capable virglrenderer and disables qemu's"
	@echo "sandbox for this guest; zvr says how to build one if it is missing."
	@echo
	@echo "Then check whether the display path is really accelerated:"
	@echo "  make -C virtualization/runner demo-gpu-check"
	@echo
	@echo "All of these work from anywhere in the repo:"
	@echo "  make -C virtualization/runner demo-status   is it running"
	@echo "  make -C virtualization/runner demo-stop     shut it down (graceful, ACPI)"
	@echo "  make -C virtualization/runner demo-reset    throw the disk away, back to the base"

## windows-demo: install Windows from an ISO and author an app for it (WIN_ISO=/path/to/Win11.iso)
# The only thing you have to supply is the Windows ISO - Microsoft's, which cannot be
# fetched for you. Everything else is defaulted to what Windows 11 requires: UEFI, Secure
# Boot, a TPM 2.0, and devices whose drivers ship in the box, because Windows Setup has none
# for virtio and reports finding no drives at all when pointed at a virtio disk.
#
# The install itself is interactive and takes a while; this target blocks until you finish
# Setup and shut the guest down, then pins the disk and authors the app against it.
WIN_ISO     ?=
WIN_APP     ?= zinc-demo-win
WIN_DISK    ?= $(IMAGE_DIR)/$(WIN_APP).qcow2
WIN_SIZE    ?= 64
WIN_MEMORY  ?= 8192
WIN_VCPUS   ?= 4
# A guest with no display driver keeps whatever mode the firmware gave it, and left alone
# that is 1280x800 for the whole life of the app. Set WIN_RES= (empty) to leave it there.
# 1824x1080 rather than 1920x1080 because an installed Windows desktop with no display driver
# paints at most 1824 columns: given 1920 it leaves the last 96 black. Measured, see the
# changelog.
WIN_RES     ?= 1824x1080
VIRTIO_WIN  := $(IMAGE_DIR)/virtio-win.iso
VIRTIO_URL  := https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso

windows-demo: build
	@test -n "$(WIN_ISO)" || { \
		echo "WIN_ISO is required - the Windows ISO is Microsoft's and cannot be fetched for you."; \
		echo "Get one from https://www.microsoft.com/software-download/windows11 then:"; \
		echo "  make -C virtualization/runner windows-demo WIN_ISO=~/Downloads/Win11.iso"; \
		exit 1; }
	@test -f "$(WIN_ISO)" || { echo "no such file: $(WIN_ISO)"; exit 1; }
	@test -x "$(ZC)" || $(MAKE) -C ../../creator build
	@mkdir -p "$(IMAGE_DIR)"
	@# The driver ISO is OPTIONAL: Windows installs onto the AHCI disk that Devices:
	@# Compatible provides and never reads it. It only matters for switching the installed
	@# guest to virtio afterwards, so a slow or failed download must not block an install.
	@if [ -n "$(SKIP_DRIVERS)" ]; then \
		echo "SKIP_DRIVERS set: installing without the virtio-win driver ISO."; \
		echo "Windows does not need it to install; fetch it later with:"; \
		echo "  make -C virtualization/runner virtio-win"; \
	elif [ ! -f "$(VIRTIO_WIN)" ]; then \
		echo "fetching the virtio-win driver ISO (~790 MB, once, resumable)."; \
		echo "Skip it with SKIP_DRIVERS=1 - Windows installs without it."; \
		echo "It is optional - Windows installs without it - and only needed later to switch"; \
		echo "the guest to virtio disk and network."; \
		curl -fSL -C - --progress-bar -o "$(VIRTIO_WIN).part" "$(VIRTIO_URL)" \
			&& mv "$(VIRTIO_WIN).part" "$(VIRTIO_WIN)" \
			|| echo "the driver ISO did not download; continuing without it (the partial file is kept)"; \
	fi
	@if [ -z "$(SKIP_DRIVERS)" ] && [ -f "$(VIRTIO_WIN)" ] && ! $(MAKE) --no-print-directory check-virtio-win >/dev/null 2>&1; then \
		echo; \
		echo "the driver ISO on disk does not verify, so it will not be attached."; \
		echo "It is KEPT, not deleted - it is a large download and yours to judge:"; \
		echo "  make -C virtualization/runner check-virtio-win   see what failed"; \
		echo "  rm $(VIRTIO_WIN)                                 discard and re-fetch later"; \
		echo "Windows installs without it, so this does not block the install."; \
		echo; \
	fi

	@echo
	@echo "Starting Windows Setup in a qemu window: UEFI + Secure Boot + TPM 2.0,"
	@echo "$(WIN_MEMORY) MiB, $(WIN_VCPUS) vCPU, a $(WIN_SIZE) GiB disk$(if $(WIN_RES), at $(WIN_RES))."
	@echo "Press a key at \"Press any key to boot from CD\" to start Setup. Windows reboots"
	@echo "itself partway through - that reboot goes to the disk on its own, so leave it be."
	@echo "When the install finishes, shut the guest down from inside Windows."
	@echo
	./bin/zvr install --disk "$(WIN_DISK)" --size $(WIN_SIZE) \
		--media "$(WIN_ISO)" $$(test -z "$(SKIP_DRIVERS)" && $(MAKE) --no-print-directory check-virtio-win >/dev/null 2>&1 && echo --media "$(VIRTIO_WIN)") \
		--firmware UEFI --secure-boot --tpm \
		$(if $(WIN_RES),--resolution $(WIN_RES)) \
		--memory $(WIN_MEMORY) --vcpus $(WIN_VCPUS)
	@echo "=== authoring $(WIN_APP) against the installed disk ==="
	@$(ZC) delete $(WIN_APP) >/dev/null 2>&1 || true
	@digest="$$(./bin/zvr pin "$(WIN_DISK)")"; \
	$(ZC) new $(WIN_APP) --vm \
		--image "$(WIN_DISK)" --base-digest "$$digest" \
		--memory $(WIN_MEMORY) --vcpus $(WIN_VCPUS) \
		--display Compatible --devices Compatible \
		--firmware UEFI --secure-boot --tpm \
		$(if $(WIN_RES),--resolution $(WIN_RES)) \
		$$(test -z "$(SKIP_DRIVERS)" && $(MAKE) --no-print-directory check-virtio-win >/dev/null 2>&1 && echo --media "$(VIRTIO_WIN)") \
		--desc "Windows guest installed by windows-demo"
	@echo
	@echo "Done. The installed disk is now this app's pinned base, so:"
	@echo "  ./bin/zvr run $(WIN_APP)      boot it (writes to its own overlay)"
	@echo "  ./bin/zvr reset $(WIN_APP)    throw the overlay away, back to freshly installed"
	@echo
	@echo "No 3D acceleration: virtio-gpu's Windows driver is display-only and passthrough"
	@echo "needs a second GPU, so this is for software that must run on Windows, not games."
	@echo "Its screen is fixed at $(if $(WIN_RES),$(WIN_RES),1280x800) - a guest with no display driver cannot be"
	@echo "told about a new mode, so resizing the window scales those pixels."
	@echo
	@echo "To lift that: boot the guest, open the CD labelled cidata and run zinc-setup.cmd."
	@echo "It stages the display, disk and network drivers. Then re-author the app with"
	@echo "--display Window (and no --resolution) for a display that resizes with the window,"
	@echo "and --devices Virtio for a faster disk and NIC."


## virtio-win: fetch the driver ISO on its own (optional; only needed to switch a guest to virtio)
virtio-win:
	@mkdir -p "$(IMAGE_DIR)"
	@test ! -f "$(VIRTIO_WIN)" || { echo "already have $(VIRTIO_WIN)"; exit 0; }
	curl -fSL -C - --progress-bar -o "$(VIRTIO_WIN).part" "$(VIRTIO_URL)" \
		&& mv "$(VIRTIO_WIN).part" "$(VIRTIO_WIN)"
	@$(MAKE) --no-print-directory check-virtio-win

## check-virtio-win: verify the driver ISO against its pin, and check its drivers are intact
# Two different checks, and it is worth being clear which is which.
#
# The pin is the security one. VIRTIO_URL is a floating "stable-virtio" path, so anyone who
# can serve it substitutes the ISO that gets mounted into a Windows guest, whose zinc-setup.cmd
# then installs drivers from it. VIRTIO_DIGEST is EMPTY by default because this project does
# not vendor a hash it has not verified against the publisher; set it once from the value
# printed below and the check becomes real. Contrast FEDORA_DIGEST above, which is pinned.
#
# The structural checks are the corruption one. An interrupted-and-resumed transfer can corrupt
# the middle of the file while leaving the ISO metadata perfectly readable, so this extracts two
# drivers Windows would actually load and checks they are real Windows binaries. That says
# nothing about provenance, and the output no longer claims it does.
VIRTIO_DIGEST ?=

check-virtio-win:
	@test -f "$(VIRTIO_WIN)" || { echo "no driver ISO at $(VIRTIO_WIN)"; exit 1; }
	@got="sha256:$$(sha256sum "$(VIRTIO_WIN)" | cut -d' ' -f1)"; \
	if [ -n "$(VIRTIO_DIGEST)" ]; then \
		if [ "$$got" != "$(VIRTIO_DIGEST)" ]; then \
			echo "the driver ISO does not match its pin:"; \
			echo "  authorised: $(VIRTIO_DIGEST)"; \
			echo "  on disk:    $$got"; exit 1; \
		fi; \
		echo "driver ISO matches its pinned digest"; \
	else \
		echo "note: this driver ISO is NOT pinned. $(VIRTIO_URL) is a floating path,"; \
		echo "      so nothing here proves it is the one the publisher intended."; \
		echo "      to pin it, check this against virtio-win's published checksum and set:"; \
		echo "        VIRTIO_DIGEST := $$got"; \
	fi
	@xorriso -indev "$(VIRTIO_WIN)" -toc >/dev/null 2>&1 || { echo "not readable as an image"; exit 1; }
	@tmp="$$(mktemp -d)"; trap 'chmod -R u+w "$$tmp" 2>/dev/null; rm -rf "$$tmp"' EXIT; \
	for drv in /viostor/w11/amd64/viostor.sys /NetKVM/w11/amd64/netkvm.sys; do \
		xorriso -osirrox on -indev "$(VIRTIO_WIN)" -extract "$$drv" "$$tmp/d.sys" >/dev/null 2>&1; \
		test "$$(head -c 2 "$$tmp/d.sys" | xxd -p)" = "4d5a" || { \
			echo "$$drv is not a Windows binary - the image is corrupt"; exit 1; }; \
		chmod u+w "$$tmp/d.sys" 2>/dev/null; rm -f "$$tmp/d.sys"; \
	done
	@echo "driver ISO is readable and its drivers are intact (this is a corruption check, not provenance)"

## virgl-venus: build the venus-capable virglrenderer that guest Vulkan needs
# Distributions ship virglrenderer built WITHOUT venus (Fedora 43's has no venus symbols at
# all), so an app with Vulkan: true needs one built here. It installs into the user's own
# data directory and never touches the system copy, so nothing else on the machine changes.
VIRGL_VERSION ?= 1.3.0
# The commit virglrenderer-1.3.0 pointed at when this was pinned. A TAG IS A MUTABLE REF:
# whoever controls the upstream repository (or a TLS middlebox on the way to it) can move it,
# and this target then runs that repository's own meson and ninja scripts on the host, as the
# user. What it produces is a library qemu dlopens - and for a Vulkan app qemu runs with its
# seccomp sandbox disabled, so there is no second boundary behind it. zvr prints this exact
# command as the fix when a Vulkan app cannot find the library (domain/paths/paths.go), so it
# is a command users are actively told to run. Pin the commit and check it.
#
# To move to a new release: git ls-remote --tags <url>, take the '^{}' (peeled) hash of the
# tag, and update both lines together.
VIRGL_COMMIT  ?= ca50e008863837e094747a69974dde3ae148aeaa
VIRGL_PREFIX  ?= $(HOME)/.local/share/zinc/virgl-venus
VIRGL_SRC     ?= $(HOME)/.cache/zinc/virglrenderer

virgl-venus:
	@for tool in git meson ninja pkg-config; do \
		command -v $$tool >/dev/null || { echo "$$tool is required to build virglrenderer"; exit 1; }; \
	done
	@pkg-config --exists vulkan libdrm epoxy || { echo "missing dev headers: need vulkan, libdrm and epoxy"; exit 1; }
	@if [ ! -d "$(VIRGL_SRC)/.git" ]; then \
		mkdir -p "$$(dirname "$(VIRGL_SRC)")"; \
		git clone --depth 1 --branch virglrenderer-$(VIRGL_VERSION) \
			https://gitlab.freedesktop.org/virgl/virglrenderer.git "$(VIRGL_SRC)"; \
	fi
	@got="$$(git -C "$(VIRGL_SRC)" rev-parse HEAD)"; \
	if [ "$$got" != "$(VIRGL_COMMIT)" ]; then \
		echo "virglrenderer is not the pinned commit:"; \
		echo "  authorised: $(VIRGL_COMMIT)"; \
		echo "  checked out: $$got"; \
		echo "the tag moved, or $(VIRGL_SRC) is a stale checkout of another version."; \
		echo "delete $(VIRGL_SRC) and retry; if it still differs, the upstream tag was rewritten."; \
		exit 1; \
	fi
	@cd "$(VIRGL_SRC)" && rm -rf build && \
		meson setup build --prefix="$(VIRGL_PREFIX)" -Dvenus=true -Dbuildtype=release >/dev/null && \
		ninja -C build >/dev/null && ninja -C build install >/dev/null
	@test -f "$(VIRGL_PREFIX)/lib64/libvirglrenderer.so.1" || { echo "build produced no library"; exit 1; }
	@test -x "$(VIRGL_PREFIX)/libexec/virgl_render_server" || { echo "build produced no render server"; exit 1; }
	@echo "venus-capable virglrenderer installed at $(VIRGL_PREFIX)"
	@echo "zvr finds it there automatically; ZVR_VIRGL_PREFIX overrides it."

## demo-status: what the demo guest is doing
demo-status: build
	@./bin/zvr status $(DEMO_APP)

## demo-stop: shut the demo guest down, gracefully
demo-stop: build
	@./bin/zvr stop $(DEMO_APP)

## demo-reset: delete the demo guest's disk, returning it to the pinned base image
demo-reset: build
	@./bin/zvr reset $(DEMO_APP)

## demo-gpu-check: ask the demo guest whether its 3D is really accelerated
# glxinfo alone cannot answer this on a cloud image: it needs a display server the guest
# does not run. These checks work headless - the driver binding, the render node, and the
# Vulkan driver name, which says "venus" when the guest is really talking to the host GPU.
demo-gpu-check:
	@echo "== guest kernel driver =="
	@ssh -p $(DEMO_PORT) -o StrictHostKeyChecking=no -o LogLevel=ERROR zinc@127.0.0.1 \
		'lsmod | grep -E "^virtio_gpu" || echo "virtio_gpu NOT loaded"'
	@echo "== render node =="
	@ssh -p $(DEMO_PORT) -o StrictHostKeyChecking=no -o LogLevel=ERROR zinc@127.0.0.1 \
		'ls -1 /dev/dri/ 2>/dev/null || echo "no /dev/dri - the guest sees no GPU"'
	@echo "== vulkan driver (venus = talking to the host GPU) =="
	@ssh -p $(DEMO_PORT) -o StrictHostKeyChecking=no -o LogLevel=ERROR zinc@127.0.0.1 \
		'vulkaninfo --summary 2>/dev/null | grep -iE "driverName|deviceName|driverInfo" || echo "vulkaninfo unavailable"'
	@echo "== opengl renderer via EGL, no display server needed =="
	@ssh -p $(DEMO_PORT) -o StrictHostKeyChecking=no -o LogLevel=ERROR zinc@127.0.0.1 \
		'eglinfo -B 2>/dev/null | grep -iE "renderer|device" || echo "eglinfo unavailable"'
	@echo "== actually rendering frames (offscreen, so no desktop is needed) =="
	@ssh -p $(DEMO_PORT) -o StrictHostKeyChecking=no -o LogLevel=ERROR zinc@127.0.0.1 \
		'glmark2 --off-screen -b build 2>&1 | tail -8 || echo "glmark2 unavailable"'
	@echo
	@echo "virgl / virtio_gpu / venus mean the accelerated path is live."
	@echo "llvmpipe or software rasterizer means the guest fell back to CPU rendering,"
	@echo "and the glmark2 score will be an order of magnitude lower."
	@echo
	@echo "To SEE 3D rather than measure it, log in at the qemu window and run:"
	@echo "  weston --backend=drm-backend.so     # a compositor, drawn through virtio-gpu"
	@echo "then inside it open weston-terminal and run  glmark2  or  glxgears."
	@echo "A Fedora Cloud image has no desktop of its own, which is why the window shows"
	@echo "only a text console until you start one."

.PHONY: demo demo-status demo-stop demo-reset demo-gpu-check virgl-venus windows-demo check-virtio-win virtio-win
