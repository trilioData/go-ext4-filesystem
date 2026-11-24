#!/bin/bash
# Fill a mounted ext4 filesystem with every kind of entry the reader must
# handle. Runs as root so it can create device nodes, set owners and write
# trusted.* / security.* attributes.
#
# usage: e2e-populate.sh <mountpoint> [big]
set -euo pipefail
root=${1:?mountpoint}
big=${2:-}
cd "$root"

rnd() { head -c "$1" /dev/urandom; }

# Regular files around block boundaries.
mkdir sizes
for n in 0 1 59 60 61 1023 1024 1025 4095 4096 4097 65536 1048576; do
  rnd "$n" > "sizes/f$n"
done

# Sparse: islands of data with holes, a hole at the end, and fully empty.
mkdir sparse
truncate -s 1G sparse/holes
for off in 0 300 700; do rnd 8192 | dd of=sparse/holes bs=1M seek="$off" conv=notrunc status=none; done
truncate -s 64M sparse/all-hole
# 64 islands: more extents than fit in the inode, so the extent tree gets an
# index level.
for i in $(seq 0 63); do rnd 4096 | dd of=sparse/fragmented bs=64K seek="$i" conv=notrunc status=none; done

# Large contiguous file across the 32768-block maximum extent length.
if [ -n "$big" ]; then
  mkdir large
  rnd $((130 * 1024 * 1024)) > large/130m.bin
fi

# Directories: deep, empty, large (hash-tree indexed by the kernel).
d=deep; for i in $(seq 1 30); do d="$d/l$i"; done
mkdir -p "$d" && echo leaf > "$d/leaf"
mkdir empty-dir
mkdir bigdir
for i in $(seq 1 5000); do : > "bigdir/entry-$i-$(printf 'x%.0s' $(seq 1 $((i % 30))))"; done

# Names.
mkdir names
echo long > "names/$(printf 'n%.0s' $(seq 1 255))"
echo unicode > "names/ünïcødé 文件.txt"
echo hidden > names/.hidden
echo spaces > "names/with  spaces and	tab"

# Symlinks: fast and slow, relative and absolute, broken, to a directory, loop.
mkdir -p links/sub
echo target > links/target
echo inner > links/sub/inner
ln -s target links/fast-rel
ln -s ../target links/sub/up
ln -s /links/target links/abs
ln -s does-not-exist links/broken
ln -s sub links/to-dir
ln -s loop-b links/loop-a
ln -s loop-a links/loop-b
ln -s "$(printf './%.0s' $(seq 1 60))target" links/slow-rel
ln -s "/$(printf 'component/%.0s' $(seq 1 30))end" links/slow-abs

# Hard links, across directories.
mkdir -p hard/other
rnd 10000 > hard/a
ln hard/a hard/b
ln hard/a hard/other/c

# Special files.
mkdir special
mkfifo special/fifo
mknod special/null c 1 3
mknod special/loop b 7 0
mknod special/big-numbers c 300 70000   # needs the new 32-bit encoding
python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])' special/socket

# Owners, including ids above 65535.
mkdir owners
for id in 0 1 1000 65534 65535 65536 100000 4294967294; do
  echo "$id" > "owners/u$id"; chown "$id:$id" "owners/u$id"
done
chown -h 1234:5678 links/fast-rel

# Modes.
mkdir modes
for m in 0000 0444 0600 0755 4755 2755 6755 1777 0777; do
  echo "$m" > "modes/m$m"; chmod "$m" "modes/m$m"
done
mkdir modes/sticky-dir && chmod 1777 modes/sticky-dir
mkdir modes/setgid-dir && chmod 2775 modes/setgid-dir

# Extended attributes.
mkdir xattr
echo s > xattr/small;  setfattr -n user.a -v 1 xattr/small
# 700 bytes: past the inline room of a 512-byte inode, inside a 1 KiB block
echo l > xattr/large;  setfattr -n user.blob -v "0x$(rnd 700 | od -An -tx1 | tr -d ' \n')" xattr/large
echo m > xattr/many
for i in $(seq 1 15); do setfattr -n "user.attr$i" -v "$(printf 'v%.0s' $(seq 1 $i))" xattr/many; done
echo e > xattr/empty;  setfattr -n user.empty xattr/empty
echo t > xattr/trusted; setfattr -n trusted.test -v trusted-value xattr/trusted
echo c > xattr/security; setfattr -n security.test -v security-value xattr/security
echo r > xattr/readonly; setfattr -n user.tag -v ro xattr/readonly; chmod 0444 xattr/readonly
setfattr -n user.on-dir -v yes xattr

# Values too large for the xattr block, stored in EA inodes. Only with the
# ea_inode feature: without it the kernel refuses them (ENOSPC). Values are
# passed base64-encoded ("0s"): a 64 KiB value in hex would be one argument
# longer than the kernel's 128 KiB limit for a single argument.
#
# EXT4_E2E_EA_INODE=1 says the filesystem has ea_inode (e2e-clone.sh sets it
# from the image); told 1, the script fails rather than go on without the
# large values. Unset or any other value, no large values are added.
if [ "${EXT4_E2E_EA_INODE:-}" = 1 ]; then
  b64() { rnd "$1" | base64 -w0; }
  big_fail() { echo "e2e-populate: the filesystem has ea_inode but $*" >&2; exit 1; }
  # setbig <path> <name> <bytes> [base64 value]: set the value (random when
  # not given) and check that it reads back with that many bytes.
  setbig() {
    local v n
    v=${4:-$(b64 "$3")}
    setfattr -n "$2" -v "0s$v" "$1" || big_fail "setting $2 ($3 bytes) on $1 failed"
    n=$(getfattr --only-values -n "$2" "$1" | wc -c) || big_fail "reading $2 back from $1 failed"
    [ "$n" = "$3" ] || big_fail "$2 on $1 reads back $n bytes, not $3"
  }
  echo 8k > xattr/big8k;    setbig xattr/big8k user.blob 8192
  echo 64k > xattr/big64k;  setbig xattr/big64k user.blob 65536
  echo many > xattr/bigmany
  for i in 1 2 3; do setbig xattr/bigmany "user.blob$i" 20000; done
  # In the inode (with 512-byte inodes), in the xattr block, in an EA inode.
  echo mixed > xattr/bigmixed
  setbig xattr/bigmixed user.small 10
  setbig xattr/bigmixed user.medium 3000
  setbig xattr/bigmixed user.large 16384
  # Identical large values, which the kernel may store in one shared EA inode.
  shared=$(b64 8192)
  echo a > xattr/shared-a;  setbig xattr/shared-a user.blob 8192 "$shared"
  echo b > xattr/shared-b;  setbig xattr/shared-b user.blob 8192 "$shared"
  mkdir xattr/big-dir;      setbig xattr/big-dir user.blob 8192
  echo "ea_inode: added large xattr values"
fi

# POSIX ACLs: access ACLs with named users and groups, a default ACL on a
# directory, and a file created under it (inherits).
mkdir acl
echo a > acl/file; setfacl -m u:1234:rw,g:4321:r,u:65536:x acl/file
mkdir acl/dir; setfacl -d -m u:1234:rwx,g::rx acl/dir; setfacl -m u:1234:rwx acl/dir
echo i > acl/dir/inherited

# Timestamps with nanoseconds, before 1970 and after 2038.
mkdir times
echo a > times/ns;     touch -h -d '2001-02-03 04:05:06.123456789' times/ns
echo b > times/pre70;  touch -h -d '1965-06-01 00:00:00.5' times/pre70
echo c > times/y2100;  touch -h -d '2100-01-01 00:00:00.000000001' times/y2100
touch -h -d '1999-12-31 23:59:59.999999999' links/fast-rel

echo "populated: $(find . | wc -l) entries, $(du -sh . | cut -f1)"
