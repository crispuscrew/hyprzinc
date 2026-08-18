#!/bin/sh
# The scratch-volume scenario: report what the kernel gave this app for a volume the config
# asked for but named no host path for, then block so the test can read the logs.
#
# Reading the mount back rather than trusting the argv is the point, as it is for capped.sh.
# A Volume with no HostMount produced no argument at all until 0.10.0, so SizeLimited and
# SizeLimitMiB validated and did nothing; only the kernel can say the ceiling is real.
echo "SCRATCH_FS=$(awk '$2 == "/scratch" { print $3 }' /proc/mounts)"
echo "SCRATCH_OPTS=$(awk '$2 == "/scratch" { print $4 }' /proc/mounts)"
echo "SCRATCH_MB=$(df -m /scratch | awk 'NR == 2 { print $2 }')"

# The limit has to bite, not merely be reported: write past it and report where it stopped.
dd if=/dev/zero of=/scratch/fill bs=1M count=32 2>/dev/null
echo "WROTE_MB=$(du -m /scratch/fill 2>/dev/null | awk '{ print $1 }')"
rm -f /scratch/fill

# The read-only one must refuse a write outright.
if touch /readonly/probe 2>/dev/null; then
	echo "READONLY=writable"
else
	echo "READONLY=refused"
fi

echo "scratch up"
exec sleep 300
