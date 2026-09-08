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
| `ext4/direntcache.go` | **new** | the directory-listing cache (§5) |
| `ext4/const.go` | modified | directory-entry file-type constants (§6) |
| `ext4/file_test.go` | modified | tests for the accessors in §1 |
| `ext4/ext4_test.go` | modified | tests for the address check in §7; block count added to fixtures |

Roughly 950 lines added across the fork.

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
`inline_data`, `ea_inode`, `meta_bg`, `bigalloc`, and `needs_recovery` — the
last being a filesystem whose journal holds unreplayed changes, where reading
without replaying returns stale data.

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
rather than returning an empty value.

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

## 8. Results

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

## 9. Correctness

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

File contents are checked against checksums taken from the source tree before
it was written into the image, and metadata against `debugfs`, the reference
tool from e2fsprogs.

All of it passes.
