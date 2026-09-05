package ext4

import (
	"encoding/binary"
	"sort"

	"golang.org/x/xerrors"
)

// Extended attribute on-disk constants. See ext2_ext_attr.h in e2fsprogs.
const (
	// Magic word at the start of both storage areas.
	xattrMagic = 0xEA020000

	// Size of struct ext2_ext_attr_entry.
	xattrEntrySize = 16

	// Size of struct ext2_ext_attr_header, which prefixes the external
	// attribute block only.
	xattrHeaderSize = 32

	// EXT2_GOOD_OLD_INODE_SIZE. Below this an inode has no i_extra_isize
	// field, so it can hold no inline attributes.
	goodOldInodeSize = 128

	// Offset of i_extra_isize within the inode.
	extraIsizeOffset = 128

	// Largest value we will accept, matching the kernel's XATTR_SIZE_MAX.
	xattrValueMax = 64 * 1024
)

// Attribute names are stored with a common prefix replaced by a one-byte
// index. Indexes 2, 3 and 8 are complete names rather than prefixes, and
// their entries normally carry a zero-length name.
//
// Table from find_ea_prefix in e2fsprogs lib/ext2fs/ext_attr.c.
var xattrPrefixes = map[byte]string{
	1:  "user.",
	2:  "system.posix_acl_access",
	3:  "system.posix_acl_default",
	4:  "trusted.",
	6:  "security.",
	7:  "system.",
	8:  "system.richacl",
	10: "gnu.",
}

// inodeOffset returns the byte offset of an inode within the filesystem.
func (ext4 *FileSystem) inodeOffset(inodeAddress int64) (int64, error) {
	bgdIndex := (inodeAddress - 1) / int64(ext4.sb.InodePerGroup)
	if bgdIndex < 0 || bgdIndex >= int64(len(ext4.gds)) {
		return 0, xerrors.Errorf("inode %d: block group index %d out of range (%d groups)",
			inodeAddress, bgdIndex, len(ext4.gds))
	}
	bgd := ext4.gds[bgdIndex]
	index := (inodeAddress - 1) % int64(ext4.sb.InodePerGroup)
	return bgd.GetInodeTableLoc(ext4.sb.FeatureInCompat64bit())*ext4.sb.GetBlockSize() +
		index*int64(ext4.sb.InodeSize), nil
}

// ListXattrs returns every extended attribute on an inode, keyed by full
// name, with the raw value bytes.
//
// Attributes live in one or two places and both are read:
//
//   - inline, in the unused tail of the inode past i_extra_isize
//   - in a separate block pointed at by i_file_acl
//
// A nil map with a nil error means the inode has no attributes.
func (ext4 *FileSystem) ListXattrs(ino int64) (map[string][]byte, error) {
	inodeSize := int(ext4.sb.InodeSize)

	off, err := ext4.inodeOffset(ino)
	if err != nil {
		return nil, err
	}

	raw := make([]byte, inodeSize)
	if _, err := ext4.r.ReadAt(raw, off); err != nil {
		return nil, xerrors.Errorf("failed to read inode %d for xattrs: %w", ino, err)
	}

	out := make(map[string][]byte)

	if err := ext4.readInlineXattrs(raw, out); err != nil {
		return nil, xerrors.Errorf("inode %d inline xattrs: %w", ino, err)
	}
	if err := ext4.readBlockXattrs(raw, out); err != nil {
		return nil, xerrors.Errorf("inode %d xattr block: %w", ino, err)
	}

	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// readInlineXattrs reads attributes stored in the inode body.
//
// Layout, from read_ea_inode_block in e2fsprogs:
//
//	inode + 128 + i_extra_isize        4-byte magic
//	inode + 128 + i_extra_isize + 4    entry table, then values
//
// Value offsets here are relative to the start of the entry table.
func (ext4 *FileSystem) readInlineXattrs(raw []byte, out map[string][]byte) error {
	inodeSize := len(raw)
	if inodeSize <= goodOldInodeSize {
		return nil // no room for i_extra_isize, let alone attributes
	}

	extraIsize := int(binary.LittleEndian.Uint16(raw[extraIsizeOffset:]))

	// Guards taken from e2fsprogs: too small to hold its own length field,
	// or no room left for a magic word after it.
	if extraIsize < 2 || inodeSize <= goodOldInodeSize+extraIsize+4 {
		return nil
	}
	if extraIsize%4 != 0 {
		return xerrors.Errorf("i_extra_isize %d is not 4-byte aligned", extraIsize)
	}

	magicAt := goodOldInodeSize + extraIsize
	if binary.LittleEndian.Uint32(raw[magicAt:]) != xattrMagic {
		return nil // no inline attributes
	}

	// The region starts just past the magic word, and the entry table
	// starts at its beginning -- so value offsets are measured from the
	// same point the entries begin.
	region := raw[magicAt+4:]
	return parseXattrEntries(region, 0, len(region), ext4.sb.FeatureIncompatEaInode(), out)
}

// readBlockXattrs reads attributes stored in a separate block.
//
// Layout:
//
//	block + 0    32-byte ext2_ext_attr_header, starting with the magic
//	block + 32   entry table, then values
//
// Note the difference from the inline case: value offsets here are relative
// to the start of the BLOCK, not the start of the entry table.
func (ext4 *FileSystem) readBlockXattrs(raw []byte, out map[string][]byte) error {
	// i_file_acl is split: the low 32 bits at offset 104, the high 16 bits
	// inside the osd2 union at offset 118.
	aclLo := binary.LittleEndian.Uint32(raw[104:])
	aclHi := binary.LittleEndian.Uint16(raw[118:])
	blk := int64(aclLo) | int64(aclHi)<<32
	if blk == 0 {
		return nil // no attribute block
	}

	blockSize := ext4.sb.GetBlockSize()
	if blk < int64(ext4.sb.FirstDataBlock) || blk >= ext4.sb.GetBlockCount() {
		return xerrors.Errorf("xattr block %d out of range", blk)
	}

	block := make([]byte, blockSize)
	if _, err := ext4.r.ReadAt(block, blk*blockSize); err != nil {
		return xerrors.Errorf("failed to read xattr block %d: %w", blk, err)
	}

	if binary.LittleEndian.Uint32(block) != xattrMagic {
		return xerrors.Errorf("xattr block %d has bad magic 0x%08x",
			blk, binary.LittleEndian.Uint32(block))
	}

	// entriesBase is 32 because value offsets are measured from the block
	// start while entries begin after the header.
	return parseXattrEntries(block, xattrHeaderSize, int(blockSize)-xattrHeaderSize,
		ext4.sb.FeatureIncompatEaInode(), out)
}

// entryLen is EXT2_EXT_ATTR_LEN: the on-disk size of one entry, its name
// included, rounded up to a 4-byte boundary.
func entryLen(nameLen int) int {
	return (nameLen + 3 + xattrEntrySize) & ^3
}

// parseXattrEntries walks an entry table and appends what it finds to out.
//
// region is the whole storage area that value offsets index into.
// entriesBase is where the entry table starts within it -- 0 for the inline
// case, 32 for a block. storageSize is how much of the region the entry table
// and its values may occupy.
func parseXattrEntries(region []byte, entriesBase, storageSize int,
	eaInodeFeature bool, out map[string][]byte) error {

	if entriesBase+storageSize > len(region) {
		return xerrors.Errorf("xattr storage size %d overruns region %d",
			entriesBase+storageSize, len(region))
	}
	entries := region[entriesBase : entriesBase+storageSize]

	// First pass: find where the entry table ends. The end offset is needed
	// to reject a value that would overlap the table itself.
	endPos, err := findXattrTableEnd(entries, storageSize)
	if err != nil {
		return err
	}

	// Second pass: extract.
	pos, remain := 0, storageSize
	for remain >= xattrEntrySize {
		e := entries[pos:]
		if binary.LittleEndian.Uint32(e) == 0 {
			break // terminator
		}

		nameLen := int(e[0])
		nameIndex := e[1]
		valueOffs := int(binary.LittleEndian.Uint16(e[2:]))
		valueInum := binary.LittleEndian.Uint32(e[4:])
		valueSize := int(binary.LittleEndian.Uint32(e[8:]))

		remain -= xattrEntrySize
		nameSpace := (nameLen + 3) & ^3
		if nameSpace > remain {
			return xerrors.Errorf("xattr name length %d overruns storage", nameLen)
		}
		remain -= nameSpace

		if pos+xattrEntrySize+nameLen > len(entries) {
			return xerrors.Errorf("xattr name at offset %d overruns table", pos)
		}
		name := xattrPrefixes[nameIndex] + string(entries[pos+xattrEntrySize:pos+xattrEntrySize+nameLen])

		switch {
		case valueInum != 0:
			// The value lives in a separate inode. Only legal with the
			// ea_inode feature, which the feature gate rejects -- so this
			// is an error rather than a silently empty value.
			if !eaInodeFeature {
				return xerrors.Errorf("xattr %q has value inode %d but ea_inode feature is not set",
					name, valueInum)
			}
			return xerrors.Errorf("xattr %q is stored in inode %d: ea_inode is not supported",
				name, valueInum)

		default:
			if valueSize > remain {
				return xerrors.Errorf("xattr %q value size %d exceeds remaining storage %d",
					name, valueSize, remain)
			}
			if valueSize > xattrValueMax {
				return xerrors.Errorf("xattr %q value size %d exceeds maximum %d",
					name, valueSize, xattrValueMax)
			}
			if valueOffs+valueSize > len(region) {
				return xerrors.Errorf("xattr %q value at %d+%d overruns region %d",
					name, valueOffs, valueSize, len(region))
			}
			// A value must sit past the end of the entry table, plus the
			// 4-byte terminator.
			if valueSize > 0 && valueOffs < entriesBase+endPos+4 {
				return xerrors.Errorf("xattr %q value offset %d overlaps the entry table",
					name, valueOffs)
			}
			remain -= valueSize

			value := make([]byte, valueSize)
			copy(value, region[valueOffs:valueOffs+valueSize])
			out[name] = value
		}

		pos += entryLen(nameLen)
	}
	return nil
}

// findXattrTableEnd returns the offset just past the last entry, mirroring the
// first loop of read_xattrs_from_buffer in e2fsprogs.
func findXattrTableEnd(entries []byte, storageSize int) (int, error) {
	pos, remain := 0, storageSize
	for remain >= xattrEntrySize {
		if binary.LittleEndian.Uint32(entries[pos:]) == 0 {
			break
		}
		nameLen := int(entries[pos])
		remain -= xattrEntrySize
		nameSpace := (nameLen + 3) & ^3
		if nameSpace > remain {
			return 0, xerrors.Errorf("xattr name length %d overruns storage", nameLen)
		}
		remain -= nameSpace
		pos += entryLen(nameLen)
	}
	return pos, nil
}

// XattrNames returns the attribute names on an inode, sorted. Convenience for
// callers that only need the names.
func (ext4 *FileSystem) XattrNames(ino int64) ([]string, error) {
	m, err := ext4.ListXattrs(ino)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}
