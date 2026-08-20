# How a release tag is made.
#
# A tag on its own says nothing about who made it, so it is signed by the person cutting it and
# this refuses rather than falling back to an unsigned one: a release that silently was not
# signed is worse than one that failed to be, since only the first is invisible.
#
# Checksums are NOT made here. They are produced by CI from the tag, because a checksum written
# on the machine that also built the binaries proves only that the machine agrees with itself.
# The builds are reproducible - `make repro` in any tool builds twice and asserts identical
# bytes - so anyone can rebuild and compare against what CI published.
#
# Usage:
#   make -f release.mk tag VERSION=0.10.0

.PHONY: tag

## tag: create the signed, annotated tag for a release
tag:
	@test -n "$(VERSION)" || { echo "usage: make -f release.mk tag VERSION=0.10.0" >&2; exit 1; }
	@git config --get user.signingkey >/dev/null 2>&1 || \
	  { echo "no git user.signingkey configured, so this tag could not be signed." >&2; \
	    echo "set one (git config user.signingkey <key>, and gpg.format=ssh for an ssh key)," >&2; \
	    echo "or tag by hand if this release is deliberately unsigned." >&2; exit 1; }
	git tag -s "v$(VERSION)" -m "Zinc $(VERSION)"
	@echo "created signed tag v$(VERSION); push it with: git push origin v$(VERSION)"
	@git tag -v "v$(VERSION)"
