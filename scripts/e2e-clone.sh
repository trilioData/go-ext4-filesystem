#!/bin/bash
# End-to-end clone test.
#
#   1. Format an ext4 disk image with mkfs.ext4.
#   2. Mount it and populate it through the kernel (scripts/e2e-populate.sh).
#   3. Remount it read-only as the reference.
#   4. Read the image with this library and write a clone (go test -tags e2e).
#   5. Compare clone and reference: in Go, and independently with rsync.
#
# Needs root (mount, mknod, chown). Usage:
#
#   sudo MKFS_OPTS="-I 512" scripts/e2e-clone.sh [big]
#
# MKFS_OPTS is passed to mkfs.ext4; empty means the distribution defaults.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

repo=$(cd "$(dirname "$0")/.." && pwd)
big=${1:-}
work=$(mktemp -d /tmp/ext4-e2e.XXXXXX)
img=$work/disk.img src=$work/src clone=$work/clone
cleanup() {
  umount "$src" 2>/dev/null || true
  umount "$work/clonefs" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

size=512M; [ -n "$big" ] && size=1G
truncate -s "$size" "$img"
# shellcheck disable=SC2086
mkfs.ext4 -F -q ${MKFS_OPTS:-} "$img"
dumpe2fs -h "$img" 2>/dev/null | grep -E "features|Block size|Inode size"

mkdir -p "$src"
mount -o loop "$img" "$src"
bash "$repo/scripts/e2e-populate.sh" "$src" "$big"
umount "$src"
e2fsck -fn "$img"
mount -o loop,ro "$img" "$src"

# With ea_inode the source holds xattr values up to 64 KiB, which an ext4
# without the feature refuses (ENOSPC), and /tmp may be one. Clone onto an
# ext4 formatted like the source then, into a subdirectory so its own
# lost+found stays out of the comparison.
if dumpe2fs -h "$img" 2>/dev/null | grep '^Filesystem features:.*\bea_inode\b' >/dev/null; then
  truncate -s "$size" "$work/clone.img"
  # shellcheck disable=SC2086
  mkfs.ext4 -F -q ${MKFS_OPTS:-} "$work/clone.img"
  mkdir -p "$work/clonefs"
  mount -o loop "$work/clone.img" "$work/clonefs"
  clone=$work/clonefs/clone
fi

cd "$repo"
EXT4_E2E_IMAGE=$img EXT4_E2E_SOURCE=$src EXT4_E2E_CLONE=$clone \
  go test -tags e2e -count=1 -v -run TestClone ./e2e/

# Independent check. rsync compares contents (-c), hard links (-H), ACLs (-A),
# xattrs (-X), owners, modes, times and devices; -n only lists differences.
diff=$(rsync -naHAXc --numeric-ids --itemize-changes --exclude=/lost+found "$src/" "$clone/")
if [ -n "$diff" ]; then
  echo "rsync found differences:"; echo "$diff"; exit 1
fi
echo "rsync: clone identical to source"
