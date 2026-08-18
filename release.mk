# Release integrity: what a person who clones this repo can check, and how a tag is made.
#
# A tag on its own says nothing about who made it or what it built. These targets close the two
# halves of that: SHA256SUMS records the bytes of every shipped binary, and the tag is signed by
# the person cutting it. Both are only worth having because the builds are already reproducible -
# `make repro` in any tool builds twice and asserts identical bytes - so a checksum is something
# anyone can regenerate rather than something they have to take on trust.
#
# Usage:
#   make -f release.mk checksums          build every tool and write SHA256SUMS
#   make -f release.mk verify             rebuild and check the bytes against SHA256SUMS
#   make -f release.mk tag VERSION=0.10.0 create the signed tag

# Each tool with the binary it produces. Named rather than globbed over bin/, which also holds
# demo fixtures and the two temporary copies `make repro` leaves behind - none of which ship.
TOOLS := creator container/runner virtualization/runner launcher/tui launcher/gui
BINARIES := creator/bin/zc container/runner/bin/zcr virtualization/runner/bin/zvr \
	launcher/tui/bin/zlt launcher/gui/bin/zlg
SUMS := SHA256SUMS

.PHONY: checksums verify tag

## checksums: build every tool in its pinned container and record the bytes it produced
checksums:
	@for tool in $(TOOLS); do \
		$(MAKE) --no-print-directory -C $$tool build >/dev/null || exit 1; \
	done
	@# Sorted and relative to the repo root, so two runs on two machines produce the same file
	@# and a difference means the bytes differ rather than the listing order.
	@printf '%s\n' $(BINARIES) | LC_ALL=C sort | xargs sha256sum > $(SUMS)
	@echo "wrote $(SUMS):"
	@cat $(SUMS)

## verify: rebuild every tool and check it against the recorded checksums
# This is what makes the file a claim rather than a note: it fails if a binary in bin/ does not
# hash to what SHA256SUMS says, which is either a stale checkout or a build that is not
# reproducible after all.
verify:
	@test -f $(SUMS) || { echo "no $(SUMS): run 'make -f release.mk checksums' first" >&2; exit 1; }
	@for tool in $(TOOLS); do \
		$(MAKE) --no-print-directory -C $$tool build >/dev/null || exit 1; \
	done
	@sha256sum --check --strict $(SUMS) && echo "VERIFIED: every binary hashes to what $(SUMS) records"

## tag: create the signed, annotated tag for a release
# Signed, because an unsigned tag is a claim anyone with push access can make and nobody can
# check. It refuses rather than falling back to an unsigned tag: a release that silently was not
# signed is worse than one that failed to be, since only the first is invisible.
tag:
	@test -n "$(VERSION)" || { echo "usage: make -f release.mk tag VERSION=0.10.0" >&2; exit 1; }
	@git config --get user.signingkey >/dev/null 2>&1 || \
		{ echo "no git user.signingkey configured, so this tag could not be signed." >&2; \
		  echo "set one (git config user.signingkey <key>, and gpg.format=ssh for an ssh key)," >&2; \
		  echo "or tag by hand if this release is deliberately unsigned." >&2; exit 1; }
	git tag -s "v$(VERSION)" -m "Zinc $(VERSION)"
	@echo "created signed tag v$(VERSION); push it with: git push origin v$(VERSION)"
	@git tag -v "v$(VERSION)"
