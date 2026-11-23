# Changes to go-ext4-filesystem

Everything this fork changes from upstream, so it can back a filesystem
restore: reading a backed-up ext4 image over NBD and writing the tree back to
a PV.

Two kinds of change. Four add capability the restore needs or close a
correctness gap. Three are performance, and between them they took a real
83,000-file restore from **10 minutes 15 seconds to 1 minute 38 seconds**.

Every section has a plain description first and a fuller one after it.

---

## 0. Inventory

| file | status | what |
| --- | --- | --- |
| `ext4/file.go` | modified | expose the inode and its number to callers (§1) |
| `ext4/features.go` | **new** | refuse filesystems using unsupported features (§2) |
| `ext4/xattr.go` | **new** | read extended attributes (§3) |
| `ext4/fs.go` | modified | call the feature gate (§2); cache directory listings (§5); cheap path resolution (§6) |
| `ext4/ext4.go` | modified | cache inodes by reference rather than by copy (§4); validate extent block addresses (§7) |
| `ext4/inode.go` | modified | correct the written/unwritten extent length boundary (§8) |
| `ext4/direntcache.go` | **new** | the directory-listing cache (§5) |
| `ext4/const.go` | modified | directory-entry file-type constants (§6) |
| `ext4/file_test.go` | modified | tests for the accessors in §1 |
| `ext4/ext4_test.go` | modified | tests for the address check in §7; block count added to fixtures |
| `ext4/inode_test.go` | modified | extent length boundary tests (§8); corrects a case that asserted the old behaviour |
| `ext4/acl.go` | **new** | convert POSIX ACLs from the ext4 disk format to the xattr format (§11) |
| `ext4/xattr.go` | modified | return ACL values in xattr format (§11) |
| `ext4/fs.go` | modified | symlink fixes: long targets on block-mapped filesystems, verbatim targets, relative resolution, loop detection, `Lstat` (§12) |
| `ext4/acl_test.go`, `ext4/image_test.go`, `ext4/api_test.go` | **new** | unit tests and tests against real ext4 images (§13) |
| `e2e/`, `scripts/e2e-*.sh` | **new** | end-to-end clone test against a kernel-mounted disk (§13) |
| `.github/workflows/test.yml` | **new** | CI on pull requests and on pushes to master/main (§13) |
| `ext4/file.go`, `ext4/fs.go` | modified | file reads: one entry per extent instead of per block, one `ReadAt` per extent, no shared read offset (§14) |
| `ext4/fs.go` | modified | refuse a superblock with zero blocks or inodes per group (§14, upstream #22) |
| `ext4/xattr.go`, `ext4/features.go`, `ext4/const.go` | modified | accept `ea_inode`; read and validate xattr values stored in EA inodes, refusing a freed one (links count 0) (§15) |
| `ext4/fs.go` | modified | fast-symlink test on `ea_inode` filesystems (§15) |
| `ext4/inode.go`, `ext4/ext4.go` | modified | refuse a block-map indirect or data block outside the filesystem, with no allocation per block on the passing path (§7, §15) |
| `ext4/xattr_test.go`, `ext4/fs_test.go`, `ext4/image_test.go` | modified | tests for §15: entry parser, EA inode reads, damaged EA inodes, symlinks, block maps |
| `scripts/e2e-*.sh`, `.github/workflows/test.yml` | modified | end-to-end clone with `ea_inode` and large xattr values (§15) |
| `e2e/clone_test.go` | modified | comment only: the block-count check's comment mentions EA inode blocks; the check is unchanged (§15) |

Roughly 4,600 lines of Go added in `ext4/` across the fork, net of lines
removed and counted against the upstream base (`fe51f5b`); about 1,270 of
them are library code and the rest tests. The end-to-end harness (`e2e/`,
`scripts/`) and CI are not counted.

---

## 1. Callers could not reach a file's identity or raw metadata

### In plain terms

**Issue.** The library described a file only through Go's standard file
interface — name, size, mode, modification time. A restore needs more than
that: which underlying object the file is (to tell that two names are the same
file), the exact ownership, and sub-second timestamps. None of it was
reachable, so a restore could not reproduce those.

**Change.** A file's underlying record and its identifying number are now
available to whoever holds the file information.

**Result.** The restore can detect hardlinks, and can read the full ownership
and timestamps rather than the trimmed versions the standard interface
exposes.

### In more detail

`FileInfo` gained `Inode()` and `InodeNumber()`, and `Sys()` now returns the
inode instead of nil, which is what `Sys()` exists for.

This matters more than it sounds. Several pieces of ext4 metadata are stored
split across two fields for historical reasons — a user ID has a low half and
a high half, a file size likewise, and timestamps keep their sub-second part
separately. Reading only the first half gives an answer that looks entirely
reasonable and is wrong: a file owned by user 100000 appears to be owned by
34464, and a 5 GB file appears to be 705 MB.

The reassembly is done by the client, but only because the library now hands
over the raw record for it to work from.

The subtle part is the sub-second timestamp. An inode records how much
extended space it declares, and the nanosecond fields exist only if that space
reaches them. Decoding those bytes unconditionally would, on a filesystem with
a small extended area, read whatever sits there — which is inline
extended-attribute data — and report it as a nanosecond count.

---

## 2. The reader accepted filesystems it could not actually read

### In plain terms

**Issue.** The library checked only that a filesystem "looked like" ext4, then
started reading. ext4 has optional features that change how data is laid out
on disk. If a backup used one the library did not implement, it would read the
wrong bytes and report success. A restore would finish, say everything was
fine, and produce corrupted files.

**Change.** Before reading anything, the library compares the filesystem's
declared features against the list it understands. Anything unknown is refused
with an error naming the feature.

**Result.** A filesystem this reader cannot handle fails immediately and says
why, instead of producing silently wrong data.

### In more detail

ext4 records its features in three groups, and the groups mean different
things:

- **incompat** — a reader that does not understand the feature must not read
  the filesystem at all.
- **ro_compat** — safe to read but not to write.
- **compat** — safe to ignore entirely.

The gate treats each accordingly: unknown incompat is a hard error, and so is
unknown ro_compat, because `bigalloc` sits in that group and genuinely does
break a reader that assumes allocation happens in blocks. Unknown compat
features are ignored, which matters because a newer `mke2fs` adds
`orphan_file` by default and rejecting it would break restores for no reason.

Notable things now refused rather than misread: `encrypt`, `casefold`,
`inline_data`, `meta_bg`, `bigalloc`, and `needs_recovery` — the last being a
filesystem whose journal holds unreplayed changes, where reading without
replaying returns stale data. `ea_inode` was on this list until §15, which
reads it.

The allowlist matches the feature set the backup writer pins explicitly, so
the two sides cannot drift apart silently.

---

## 3. Extended attributes were not readable at all

### In plain terms

**Issue.** Extended attributes are extra labels attached to files — SELinux
security contexts, access control lists, application metadata. The library had
no support for them whatsoever. A restore would produce files with the right
contents and no labels, which for something like a Kubernetes volume can mean
the restored application will not start.

**Change.** Added the ability to read a file's extended attributes.

**Result.** Attributes come back with the file. Verified against `debugfs`,
the reference tool, on attributes stored both ways ext4 stores them.

### In more detail

ext4 keeps attributes in one of two places and both had to be handled: small
ones are packed into unused space inside the inode itself, larger ones go into
a separate block the inode points at. A single file can have some of each.

The two locations use the same entry layout but measure value offsets from
different starting points — relative to the entry table in one case, relative
to the start of the block in the other. That is the detail most likely to be
got wrong, and getting it wrong yields plausible-looking garbage rather than
an error.

Attribute names are stored with common prefixes (`user.`, `security.`,
`trusted.` and so on) replaced by a one-byte code, so the full name has to be
reassembled. The bounds and consistency checks were ported from the reference
implementation, including rejecting the case where a value lives in a separate
inode — a feature the gate in §2 refuses anyway, but which now fails loudly
rather than returning an empty value. Since §15 such a value is read when the
filesystem has `ea_inode`; without the feature it is still an error.

---

## 4. The inode cache was making copies instead of sharing

### In plain terms

**Issue.** Every time the library looked up a file's metadata from its cache,
it made a complete copy and put that copy on the heap — even though the data
never changes. During one large restore this happened 851 million times, which
meant creating and throwing away about 218 gigabytes of duplicated data. Most
of the program's time went into cleaning that up.

**Change.** The cache hands out a shared reference instead of a copy.

**Result.** About 18% faster on its own, and it removed most of the memory
churn that was slowing everything else down.

### In more detail

The cache stored inode structures by value inside an interface. Reading one
back required copying 256 bytes out of the interface, and taking the address
of that copy forced it onto the heap. Both costs were paid on every hit.

Since the reader never modifies an inode, the copy served no purpose. The
cache now stores and returns pointers. The one risk this introduces is a
caller mutating a shared inode and corrupting it for everyone else, so that
constraint is documented at the call site; nothing in the package does it.

The garbage collector had been consuming roughly a fifth of all CPU, almost
entirely on these copies.

---

## 5. Directory contents were re-parsed every time they were used

### In plain terms

**Issue.** To find a file, the library reads the directory containing it and
decodes the list of names inside. It did that decoding again for every file it
looked at — so a directory holding a thousand files was decoded a thousand
times, producing the identical answer each time. Roughly 30% of all the
program's work was re-doing this.

**Change.** Decoded directory listings are remembered, so each directory is
decoded once.

**Result.** Directory lookups got about three times faster, and this was the
single largest of the three performance changes.

### In more detail

There were already two caches: one for the raw bytes read from the device and
one for file metadata. Neither covered the decoded directory listing, which is
the expensive part — the decoding walks the record fields using runtime
reflection, far slower than reading fixed-position bytes.

A cache of parsed listings, keyed by directory, now sits in the library. It is
bounded by the total number of entries held rather than the number of
directories, so one very large directory cannot consume the whole budget. The
default holds a million entries, roughly 50 MB.

Listings are shared rather than copied, with the same read-only constraint as
§4. Because a backup image is read-only for the life of the handle, there is
no invalidation to get wrong.

---

## 6. Finding one file meant reading metadata for all of its neighbours

### In plain terms

**Issue.** To find a file in a directory, the library loaded the full metadata
for every file in that directory, then picked out the one it wanted and
discarded the rest. In a directory of 2,000 files that is 2,000 pieces of work
to answer one question. Worse, it happened at every level of a path — so
looking up a file three directories deep repeated it three times.

**Change.** The directory listing already contains each entry's identifier and
its type, so the library now uses that to step to the next level directly,
without loading anything else. Full metadata is loaded only for the one file
actually being asked about.

**Result.** Looking up a file in a 2,000-entry directory went from 2.4
milliseconds to 0.016 milliseconds — about **152 times faster**.

### In more detail

Three separate operations shared this pattern: resolving a path, fetching a
single file's information, and opening a file. Each listed the containing
directory in full and scanned the result, so opening a file in a large
directory cost 2,000 metadata reads for one file.

A directory entry on disk already carries the child's identifier and, when the
`filetype` feature is present, its type. That is everything needed to descend
a level, so descending now costs no metadata read at all. Where the feature is
absent the entry carries no type, and the library falls back to one read — for
the single entry that matched, not for all of them.

This is the change that turns path resolution from work proportional to
directory size into roughly constant work.

---

## 7. A corrupt block address was read rather than refused

### In plain terms

**Issue.** A file's contents are found through a list of addresses stored on
disk saying "this file's data lives at block 4,000". The reader trusted those
addresses without checking them. An address pointing outside the filesystem
would still be read, because the region handed to the reader is normally
bigger than the filesystem itself -- a partition has unused space after its
last filesystem block, and a whole disk has more after the partition. So the
read succeeded, returned whatever bytes happened to be sitting there, and they
were written into the restored file as its contents. No error, no warning, a
restore that reported success.

**Change.** Before an address is used, it is checked against the filesystem's
own record of how many blocks it has. Anything outside that range is refused
with an error naming the address.

**Result.** A damaged or tampered-with image now fails and says which address
was wrong, instead of quietly producing a file full of unrelated bytes.

### In more detail

The addresses live in the extent tree, which has two kinds of entry, and they
were exposed differently:

- **Internal nodes** point at another block of the tree. A bad address here was
  usually caught one level down, because the next thing read is an extent
  header and the reader checks its magic number. That was luck rather than
  design, and the check only ran after a block had already been read from an
  arbitrary offset.
- **Leaf entries** point at file content, and there is no magic number in file
  data to fail against. Nothing downstream could tell a good address from a bad
  one. This was the silent path.

Both now validate against `[FirstDataBlock, block count)` from the superblock,
and the extent's length has to fit inside that range too -- otherwise a
plausible start address with an absurd length walks off the end. The check
sits in the one function that parses extents, so the four places that turn an
address into a read are all covered by it rather than each needing its own
check. It is the same bound the external attribute block was already checking.

Worth being clear about the boundary this does *not* move: the reader is given
a region, and where that region ends is a separate question from where the
filesystem ends. Bounding the region more tightly would not have helped,
because the gap between the two is exactly where a bad address lands and still
finds readable bytes. The check has to be on the address, against the
superblock.

Block-mapped (ext3-style) files were left out of that check until §15:
an indirect block, or a data block named by the inode or an indirect block,
past the end of the filesystem was read like any other. Both are now checked
against the same range before they are used. A zero pointer is still a hole.

This is not a bug that was hit. It needs a damaged or deliberately crafted
image, and the images this reader is pointed at come from a writer under the
same control. It is here because the failure mode is the one the feature gate
in section 2 exists to prevent -- a successful restore of the wrong bytes --
and closing it costs one comparison per extent.

The test fixtures needed a correction alongside it. Several hand-built
superblocks in the unit tests left the block count at zero, which declares a
filesystem with no blocks at all and makes every address in the fixture out of
range. They were relying on the absence of the check. One test that
deliberately addresses block 2^32+2, to prove 64-bit addresses are assembled
correctly, now also declares the 64bit feature and a block count past that
block -- which is what a real filesystem addressing it would have.

---

## 8. A maximum-length extent read back as 128 MiB of zeros

### In plain terms

**Issue.** A file's data is described by a list of extents -- entries saying
"this stretch of the file lives at this stretch of the disk". Each entry stores
its length in a single 16-bit number, and that number does two jobs at once: it
carries the block count, and it says whether the extent holds real data or is
reserved space that should read as zeros. ext4 does this by splitting the
number's range, not by reserving a bit -- a value up to and including 32768
means real data of exactly that length, and anything above it means reserved
space whose real length is the value minus 32768.

The reader tested bit 15 instead of comparing. Those two rules agree on every
value but one: 32768 has bit 15 set, yet it is real data of the maximum length
ext4 can describe -- 128 MiB at a 4 KiB block size. So a full-length extent was
read as "reserved, zero blocks long", dropped from the file's block map, and
every byte it covered was served as zeros.

**Change.** `IsUninitialized` compares against 32768 rather than masking bit 15,
and `GetLen` returns the value unchanged below the boundary and subtracts 32768
above it. This is what `ext4_ext_is_unwritten` and `ext4_ext_get_actual_len` in
`fs/ext4/ext4_extents.h` do.

**Result.** Files containing a maximum-length extent restore with their real
contents instead of a 128 MiB run of zeros. Nothing about the failure was
visible before: no error, no warning, correct file size, correct permissions,
correct timestamps, and `fsck` clean on the image -- only the bytes were wrong.

### In more detail

The value is not an unlikely one. Any allocator that fills an extent to
capacity produces it, and the skeleton builder does so deliberately:

```python
want = min(remaining, EXT4_MAX_EXTENT_BLOCKS)   # 32768
```

So **every file larger than 128 MiB** carries at least one such extent. Density
and sparseness are irrelevant: what matters is whether any single contiguous run
reaches 32768 blocks. A 200 MiB contiguous file has one, a 200 MiB file
fragmented into 4 MiB pieces has none.

Measured on a 3,077,318,007-byte file. Its extent list, from `debugfs`:

```
(0     - 15320 ) : 17447-32767      15,321 blocks   served correctly
(15321 - 48088 ) : 33793-66560      32,768 blocks   dropped -> zeros
(48089 - 79831 ) : 66561-98303      31,743 blocks   served correctly
(79832 - 112599) : 99329-132096     32,768 blocks   dropped -> zeros
```

`cmp` between the source file and its restored copy reported the first
difference at byte 62,754,817 -- which is block 15,321, exactly where the first
maximum-length extent begins.

Across a 64,697-entry tree, 7 files restored with wrong contents and the split
was exact: every file needing more than one extent failed, every file fitting in
one extent passed. The largest that passed was 101 MiB; the smallest that failed
was 164 MiB.

Three other call sites read the same length field, and all three were wrong in
the same way:

- `fs.go:786` builds a file's logical-to-physical block map and skips extents it
  believes are unwritten. This is the one that produced the zeros.
- `fs.go:342` and `fs.go:583` walk a directory's blocks and *reject* an
  unwritten extent with an error. A directory large enough to hold a
  maximum-length extent would have failed the restore outright rather than
  silently -- unreachable in practice, but wrong.
- `ext4.go:176` passes `GetLen()` to the address check added in §7. With the
  length reported as zero, that check validated an empty range, so §7's
  guarantee did not apply to precisely the extents where a bad address would do
  the most damage.

The behaviour was also pinned by a test. `TestExtent_IsUninitialized` carried a
case asserting that `0x8000` meant "uninitialized, 0 blocks", so the suite
agreed with the reader rather than with the on-disk format. That case is now
corrected, and `TestExtentLenBoundary` covers the whole boundary --
32766, 32767, **32768**, 32769, 40000, 65535 -- with `32768` being the row no
earlier test could have covered.

---

## 9. Results

| | before | after | |
| --- | --- | --- | --- |
| find a file in an 18-entry directory | 29.1 µs | 0.9 µs | 33× |
| find a file in a 2,000-entry directory | 2,364 µs | 15.5 µs | **152×** |
| list a 2,000-entry directory | 2,354 µs | 571 µs | 4× |
| restore the test fixture | 8.89 s | 0.71 s | 12.5× |
| **restore 83,000 files, 9.7 GB, over NBD** | **10 m 15 s** | **1 m 38 s** | **6.3×** |
| test suite | 44 s | 5 s | 9× |

### Where the time goes now

Before these changes, two thirds of all CPU went into turning a path into a
file — decoding directories and reading metadata that was then thrown away.

Now roughly three quarters of it is system calls writing the restored files to
disk, which is the actual work. Path resolution has disappeared from the
profile entirely; metadata lookups account for about 4%.

That is the healthy shape: the program is limited by moving data rather than
by finding it.

### The next thing worth doing

The restore reads and writes one file at a time. With path lookup no longer
dominant, the remaining cost is transfer and writing, and both parallelise
well — several files at once would use the network round trips and the disk
much better than a single sequential pass. That is where the next significant
gain is, rather than in further lookup work.

A smaller one: the cache still builds a text key by formatting a number, which
was 16% of CPU before and is now inside that 4%. Fixing it properly requires a
type change that reaches the calling code, so it is worth measuring again
before deciding it is worth the churn.

---

## 10. Correctness

None of the performance changes alter what the reader returns, only how much
work it does to return it. The capability changes add what was missing without
altering existing behaviour.

That is backed by a fixture-based test suite covering the cases where a reader
can be wrong while still appearing to work: hardlinks sharing an inode, both
forms of symlink storage, sparse files and files ending in a hole, users above
65535, sub-second timestamps, files over 4 GB, sizes either side of a block
boundary, directories large enough to be indexed, 255-byte and non-ASCII
names, extended attributes in both storage locations, a read-only file
carrying an attribute, and ten cases of an unsupported feature being correctly
refused.

The address check in section 7 is covered separately, by unit tests rather
than fixtures -- a valid image cannot contain a bad address, so the cases have
to be built by hand. Four of them: an address past the end, one exactly at the
end, one starting inside the filesystem but running past the end, and the
boundary that must be *accepted*, an extent ending exactly on the last block.

Section 8's extent length boundary is unit-tested for the same reason, and for
one more: the value that mattered, 32768, was not merely untested but asserted
*wrongly* by an existing case. A fixture would not have caught it either, since
producing a maximum-length extent needs a file over 128 MiB and the suite's
images are small. The boundary is now pinned at 32766, 32767, 32768, 32769,
40000 and 65535, plus a three-extent map that fails if a full-length extent is
ever dropped again.

File contents are checked against checksums taken from the source tree before
it was written into the image, and metadata against `debugfs`, the reference
tool from e2fsprogs.

All of it passes.

---

## 11. POSIX ACLs came back in a format setxattr rejects

### In plain terms

**Issue.** ext4 stores ACLs on disk in its own compact format. The kernel
converts them to the standard format whenever a program reads them. This
library reads the disk directly, so it returned the disk format, and a restore
passing that value to `setxattr` got `EOPNOTSUPP`. The ACL was lost, and
because a file with an ACL keeps the ACL mask in its group permission bits,
the restored file's group gained the mask's permissions (for example `r--`
became `rw-`).

**Change.** `ListXattrs` converts `system.posix_acl_access` and
`system.posix_acl_default` to the format `getxattr(2)` returns. Every other
value is still returned raw.

**Result.** An ACL read from an image is byte-identical to what the kernel
reports for the same file, and can be passed straight to `setxattr`.

### In more detail

On disk (`fs/ext4/acl.h`): a version-1 header, then entries of tag and
permission, with a 32-bit id only for named users and groups. The xattr format
(`posix_acl_xattr`): a version-2 header, every entry 8 bytes, and
`ACL_UNDEFINED_ID` (`0xffffffff`) where there is no id. `ext4ACLToXattr`
mirrors `ext4_acl_from_disk` and `convert_disk_buffer_to_posix_acl` in
e2fsprogs. A value already at version 2 is passed through; anything else that
does not parse is an error rather than a value that would fail later.

Found by backing up an XFS volume with an ACL and tracing the restore's
`setxattr` calls.

---

## 12. Symlinks

### In plain terms

**Issues.**

- Reading a long symlink (60 bytes or more) on a filesystem without extents
  panicked with a slice-bounds error. The code decided where the target lives
  by "does the inode use extents" instead of "is the target in the inode".
- `ReadLink` cleaned the target (`./a/../b` came back as `b`), so a restore
  recreated a different link.
- `Open` and `Stat` resolved a relative target from the filesystem root
  instead of the link's directory, so `sub/link -> ../file` opened the wrong
  path or nothing, and a symlink loop recursed until the stack overflowed.
- `Lstat` was `Stat`, so it followed the link.

**Changes.**

- A target is in the inode exactly when the link owns no data blocks other
  than an xattr block (the kernel's `ext4_inode_is_fast_symlink`). Otherwise
  it is read through the extent tree or the block map, whichever the inode
  uses. §15 adds a size test on `ea_inode` filesystems.
- `ReadLink` returns the target verbatim, as `readlink(2)` does.
- `Open` and `Stat` follow a symlink in the final component relative to the
  link's own directory, absolute targets from the root, and stop after 40
  links with `ErrSymlinkLoop`. `..` cannot climb above the root.
- `Lstat` describes the link itself.

Symlinks in intermediate path components are still not followed, as before.

---

## 13. Tests and CI

The fork now carries its own tests against real images, in addition to the
unit tests.

- **Image tests** (`ext4/image_test.go`, `ext4/api_test.go`). A source tree is
  built in a temporary directory, made into an image with `mke2fs -d` (no root
  needed), adjusted with `debugfs` for what an unprivileged user cannot create
  (owners above 65535, root-owned files, device nodes, `security.*` and
  `trusted.*` attributes), and read back. Every entry is compared with the
  source: type and all mode bits, owner, mtime, size and contents, symlink
  targets, directory listings, hardlink identity and link count, and every
  xattr byte for byte, ACLs included. Six geometries: the skeleton plugin's
  (4 KiB blocks, 512-byte inodes), stock ext4 with a journal, 1 KiB blocks,
  128-byte inodes, metadata_csum_seed (the e2fsprogs 1.47.1+ default), and ext3-style block maps. The trees include sizes either
  side of block boundaries, sparse and fragmented files (an extent tree with
  an index level), a 129 MiB file (maximum-length extent) and a 66 MiB file on
  1 KiB block maps (triple-indirect blocks), a 3,000-entry hash-tree
  directory, deep paths, 255-byte and non-ASCII names, every symlink shape
  including loops, and large and many xattrs. Further tests cover path forms,
  error values, symlink resolution, `Lstat`, a filesystem at a partition
  offset, six unsupported features refused by name (`ea_inode` is accepted
  since §15), and damaged input
  returning errors instead of panicking. `-short` skips the large files.
  The e2fsprogs tools are required; without them the tests skip, or fail when
  `EXT4_REQUIRE_TOOLS` is set.
- **End-to-end clone** (`e2e/`, `scripts/e2e-clone.sh`, root). A disk image is
  formatted with `mkfs.ext4`, populated through a kernel mount (including
  device nodes, sockets, owners up to 4294967294, nanosecond and post-2038
  timestamps, inherited default ACLs), cloned with this library by a minimal
  restorer, and compared with the kernel-mounted original, in Go and again
  with `rsync -naHAXc`.
- **CI** (`.github/workflows/test.yml`) runs on every pull request and on
  pushes to `master` and `main`: gofmt, vet, the unit and image tests with
  `-race` on Ubuntu 22.04 and 24.04 (e2fsprogs 1.46 and 1.47), and the
  end-to-end clone with stock mkfs.ext4 on ubuntu-latest and ubuntu-22.04 plus
  four pinned geometries on ubuntu-24.04 (the fourth, with `ea_inode`, from
  §15).

The feature gate now also accepts `metadata_csum_seed`. It only records where
the checksum seed lives, this reader does not verify checksums, and
`mkfs.ext4` enables it by default from e2fsprogs 1.47.1 (ubuntu-latest runners),
so stock filesystems from current distributions were being refused.

Statement coverage of `ext4/` went from 42% to 84%. Both the ACL bug (§11)
and the block-map symlink panic (§12) fail these tests on the previous code.

---

## 14. Reading file contents

### In plain terms

**Issues.**

- Opening a file built a map with one entry for every 4 KiB block and kept it
  for the whole read: about 93 MiB of memory for a 10 GiB file, about 6.5 GiB
  for a 1 TiB file. One large file in a volume could get the datamover pod
  killed for memory, with no error to explain it.
- `Read` returned at most one block per call whatever the size of the caller's
  buffer, and fetched that block from the device 512 bytes at a time: eight
  device reads and a fresh buffer per 4 KiB.
- Every read moved the one read offset that all open files share, so two files
  read at the same time could get each other's blocks. Fixed upstream (#23)
  for the same reason.
- A superblock with zero blocks per group or zero inodes per group crashed
  `NewFS` with a divide by zero (upstream #22).

**Changes.**

- A `File` keeps one entry per run of contiguous blocks: an extent, or a
  stretch of neighbouring blocks in a block-mapped file. Memory follows the
  number of extents, not the size of the file. A block no run covers is a
  hole and reads as zeros, as before; so does an unwritten extent.
- `Read` fills the caller's buffer up to the end of the current run, hole or
  file, with a single `ReadAt`. A 1 MiB buffer gets up to 1 MiB per call.
- `ReadAt` does not use the shared offset, so different files can be read
  concurrently. One `File` is still for one reader at a time.
- Extents of one file that overlap are refused when the file is opened, as the
  kernel does. Before, whichever sorted later silently won.
- `parseSuperBlock` returns an error for the zero group sizes.
- A device that ends partway through a file's data returns
  `io.ErrUnexpectedEOF`. It used to return an error wrapping `io.EOF`, which
  a caller testing with `errors.Is(err, io.EOF)` would take for a normal end
  of file and so accept a short file.

The bytes returned are unchanged. Not changed: opening a block-mapped
(ext3-style) file still resolves a 4-byte address per block first, about
256 MiB per TiB, released once the runs are built.

Upstream #21 (symlink resolution) was not taken: §12 already covers it, and
upstream cleans the link target where this fork returns it verbatim.

---

## 15. Extended attribute values stored in EA inodes (`ea_inode`)

### In plain terms

**Issue.** ext4 keeps an extended attribute value in the inode or in one
xattr block, so a value is limited to about one block (4 KiB on most
filesystems). With the `ea_inode` feature a larger value, up to 64 KiB, is
stored in a separate inode of its own (an EA inode) and the attribute entry
points at it. This reader refused every filesystem with `ea_inode` (§2), so a
volume whose files carry such values could not be restored at all. The
skeleton plugin's TK-12867 change enables `ea_inode` on the images it builds,
so those would have been refused.

**Changes.**

- `ea_inode` is accepted by the feature gate, and `ListXattrs` reads a value
  stored in an EA inode, whether the attribute entry sits in the inode or in
  the xattr block. Several entries may point at one EA inode (the kernel
  shares one between equal large values); each returned value is its own
  copy.
- Each value is checked before it is returned. An entry pointing at an EA
  inode must declare a size from 1 byte to 64 KiB. The EA inode must be a
  number from the first non-reserved inode (never below 11, whatever the
  superblock says) to the inode count; a regular file; flagged
  `EA_INODE_FL` (0x200000); not freed (links count above 0); and exactly
  as long as the entry says. Its data,
  found through its extents or its block map, must have no hole and no
  unwritten extent, and every block must lie inside the filesystem. Anything
  else is an error naming the attribute and the reason, never a panic and
  never bytes from elsewhere. The entry's value offset is not used for such
  a value, and the value takes no room in the inode or the xattr block.
- Without `ea_inode`, an entry pointing at an EA inode is still an error, as
  before.
- **Symlinks.** A short symlink keeps its target in the inode. The reader
  decided "short" as the kernel's older test does: the link owns no blocks
  other than an xattr block. On an `ea_inode` filesystem the blocks of a
  link's EA inodes are counted in its block count, so a short link carrying
  a large value looked long and its target was read from the wrong place.
  That test still runs first; when it says "long" and the filesystem has
  `ea_inode`, a target of 1 to 59 bytes is taken as short. Without
  `ea_inode` nothing changes.
- **Block maps.** Reading an EA inode through a block map exposed a gap in
  the shared block-map code: an indirect block or a data block past the end
  of the filesystem was read like any other. Both are now refused (§7). A
  damaged block map now fails the whole file read instead of returning bytes
  from outside the filesystem. A zero pointer is still a hole. The check
  builds its error text only when it fails, so it costs no allocation per
  block on the many block-mapped reads that pass it.

**Not done.** ext4 keeps a hash of the value in the EA inode and folds it
into the entry's hash. The reader does not verify it, as it verifies no
checksums anywhere. What the checks above do and do not catch:

- Detected: a pointer out of range or to a reserved inode; a pointer to an
  inode that is not a regular file or lacks `EA_INODE_FL`; a freed EA inode
  (links count 0, which e2fsprogs reports as `EXT2_ET_EA_INODE_CORRUPTED`
  and the kernel refuses; a freed inode's blocks may already hold other
  data); a size that does
  not match the entry; a hole, an unwritten extent, an extent pointing at
  physical block 0, or a block outside the filesystem in the value's data.
- Not detected: an entry pointing at a different EA inode of the same size
  (the other value's bytes are returned; `TestImageEAInodeShared` builds
  exactly this to model a shared EA inode), and damaged bytes inside the
  value's own blocks. Both would take the stored hash to notice.

### In more detail

`parseXattrEntries` takes an `eaValueReader` callback and calls it for an
entry with a non-zero `e_value_inum`, after checking the feature and the
size (0 and over 64 KiB refused, on the raw 32-bit field). A reader result
of the wrong length is refused. `e_value_offs` is not read for such an entry
and its size is not counted against the inline or block budget.
`readEAInodeValue` is the callback both call sites pass; it makes every check
listed above and allocates a fresh slice per call. The links-count check
was added after checking that valid EA inodes have a non-zero count:
`mke2fs -d` (e2fsprogs 1.46.5) writes 1, on extent-mapped and block-mapped
images, and the kernel sets 1 and keeps the sharing count in other fields;
the `ea_inode` end-to-end row reads kernel-written EA inodes through it.
The reader that refuses every EA inode, used before `ea_inode` was
supported, now lives in `ext4/xattr_test.go`, the only place still using it.

`isFastSymlink` agrees with the kernel. Linux (current master)
`ext4_inode_is_fast_symlink` decides by size alone on an `ea_inode`
filesystem (a symlink, size non-zero and below 60) without looking at
`i_blocks`; the reader's rule gives the same answer for every well-formed
link with a non-empty target (the `i_blocks` test still runs first, so a
damaged link can differ). An empty target is fast only when `i_blocks`
counts at most an xattr block, as before. e2fsprogs master
(`ext2fs_inode_has_valid_blocks2`) falls through to the size test when
`i_blocks` is non-zero.

e2fsck 1.46.5 does not. For a short symlink that owns an EA inode and has no
xattr block, once the EA blocks are charged to it, e2fsck 1.46.5 reports
"illegal block(s)": it takes the target bytes for block numbers. That is a
false positive of the old e2fsck, not damage; it is recorded here so nobody
chases it. `TestImageSymlinkWithEAInode` builds that case after its e2fsck
pass for this reason.

Tests:

- Unit tests (`ext4/xattr_test.go`) on hand-built entry tables for the
  parser's EA inode branch: missing feature, size 0, over 64 KiB, reader
  error, wrong length, and that the value takes no storage; for
  `readEAInodeValue`'s inode number range and its floor of 11; and for the
  extent error branches of the EA inode's block lookup (a block mapped twice,
  an unwritten extent, an extent at physical block 0). `ext4/fs_test.go`
  pins `isFastSymlink` with and without `ea_inode`, and the block-map bounds
  checks on hand-built indirect blocks: both rejection (past the end) and
  acceptance (the last block, count-1, and block 1 where the first data
  block is 1), and that a mapped block allocates no more than a hole
  (`testing.AllocsPerRun`).
- Image tests (`ext4/image_test.go`): values of 4040, 4041, 8192 and 65536
  bytes, two 64 KiB values on one inode, one inode with an inline, a block
  and an EA inode value, an ACL beside a large value (when the source
  directory takes ACLs), on 4 KiB blocks with
  512-byte inodes, 1 KiB blocks with 256-byte inodes and a block-mapped
  filesystem; a shared EA inode (patched in with `debugfs`, since
  `mke2fs -d` never shares one); damaged EA inodes and pointers (out of
  range, reserved inode, directory, flag missing, links count 0, size off
  by one, hole, block outside the filesystem); symlinks owning EA inodes;
  and block maps pointing past the filesystem. The source tree for the
  large values is
  built under `/dev/shm`, because the default temp directory may refuse
  values over about one block; tmpfs takes 64 KiB `user.*` values from
  Linux 6.6. Without that, or without an `ea_inode`-capable `mke2fs`, these
  tests skip, naming the reason, or fail when `EXT4_REQUIRE_TOOLS` is set.
  Both CI unit-test runners (ubuntu-22.04 and ubuntu-24.04) run kernel 6.8
  or later, so CI runs them.
- End-to-end clone: a new CI row, `skeleton layout with ea_inode`
  (`-I 512 -O ^has_journal,^orphan_file,ea_inode`, ubuntu-24.04). With
  `ea_inode`, `scripts/e2e-populate.sh` adds files with an 8 KiB, a 64 KiB,
  three 20,000-byte, and a 10/3,000/16,384-byte set of values, two files
  with the same 8 KiB value (identical values, which the kernel may store in
  one shared EA inode), and a directory with an 8 KiB value. Each value is
  read back with `getfattr` and its size checked. `scripts/e2e-clone.sh`
  detects `ea_inode` from the image and passes it in `EXT4_E2E_EA_INODE`;
  told the feature is present, the populate script fails with a message if
  a large value cannot be set, so the row cannot pass without them. Values
  go through `setfattr` in base64: in hex a 64 KiB value is a single
  argument over the kernel's 128 KiB limit. Without `ea_inode` the script
  does what it did before. `scripts/e2e-clone.sh` then clones
  onto a second ext4 formatted the same way, because an ext4 without
  `ea_inode` (as `/tmp` may be) refuses values over about one block with
  `ENOSPC`. The clone comparison's block-count check covers only large
  sparse files, so EA inode blocks counted in a file's `st_blocks` cannot
  fail it.

The end-to-end clone with `ea_inode` was not run on the machine this was
written on (no root); CI runs it.
