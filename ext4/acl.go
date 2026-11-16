package ext4

import (
	"encoding/binary"

	"golang.org/x/xerrors"
)

// POSIX ACLs are stored by ext4 in a compact on-disk format that differs from
// the format getxattr(2) and setxattr(2) use. The kernel converts between the
// two on every access (fs/ext4/acl.c); a reader that bypasses the kernel has to
// do the same, or a caller passing the value to setxattr gets EOPNOTSUPP and
// the ACL is lost.
//
// On disk (ext4_acl_header + entries):
//
//	u32 version = 1
//	ACL_USER, ACL_GROUP:                      u16 tag, u16 perm, u32 id   (8 bytes)
//	ACL_USER_OBJ, ACL_GROUP_OBJ, ACL_MASK,
//	ACL_OTHER:                                u16 tag, u16 perm           (4 bytes)
//
// xattr format (posix_acl_xattr_header + entries):
//
//	u32 version = 2
//	every entry:                              u16 tag, u16 perm, u32 id   (8 bytes)
//	entries without an id carry ACL_UNDEFINED_ID
//
// All fields are little-endian in both formats.
const (
	ext4ACLVersion       = 0x0001
	posixACLXattrVersion = 0x0002

	aclUserObj  = 0x01
	aclUser     = 0x02
	aclGroupObj = 0x04
	aclGroup    = 0x08
	aclMask     = 0x10
	aclOther    = 0x20

	aclUndefinedID = 0xFFFFFFFF

	aclHeaderSize     = 4
	aclShortEntrySize = 4
	aclLongEntrySize  = 8
)

// Names of the two attributes that hold POSIX ACLs. Their values are stored in
// the ext4 on-disk ACL format and must be converted before use.
const (
	XattrPosixACLAccess  = "system.posix_acl_access"
	XattrPosixACLDefault = "system.posix_acl_default"
)

func isPosixACLXattr(name string) bool {
	return name == XattrPosixACLAccess || name == XattrPosixACLDefault
}

// ext4ACLToXattr converts an ACL from the ext4 on-disk format to the
// posix_acl_xattr format, the bytes getxattr(2) returns for the same file on a
// mounted filesystem. It mirrors ext4_acl_from_disk followed by
// posix_acl_to_xattr in the kernel, and convert_disk_buffer_to_posix_acl in
// e2fsprogs.
//
// A value that is already in xattr format (version 2) is returned unchanged.
// Anything else that does not parse is an error: passing it on would only
// fail later, at setxattr, with no indication of why.
func ext4ACLToXattr(disk []byte) ([]byte, error) {
	if len(disk) < aclHeaderSize {
		return nil, xerrors.Errorf("ACL value of %d bytes is shorter than its header", len(disk))
	}
	switch v := binary.LittleEndian.Uint32(disk); v {
	case posixACLXattrVersion:
		out := make([]byte, len(disk))
		copy(out, disk)
		return out, nil
	case ext4ACLVersion:
	default:
		return nil, xerrors.Errorf("unknown ACL version %d", v)
	}

	out := make([]byte, aclHeaderSize, len(disk)*2)
	binary.LittleEndian.PutUint32(out, posixACLXattrVersion)

	for pos := aclHeaderSize; pos < len(disk); {
		if len(disk)-pos < aclShortEntrySize {
			return nil, xerrors.Errorf("ACL entry at offset %d is truncated", pos)
		}
		tag := binary.LittleEndian.Uint16(disk[pos:])
		perm := binary.LittleEndian.Uint16(disk[pos+2:])

		id := uint32(aclUndefinedID)
		switch tag {
		case aclUserObj, aclGroupObj, aclMask, aclOther:
			pos += aclShortEntrySize
		case aclUser, aclGroup:
			if len(disk)-pos < aclLongEntrySize {
				return nil, xerrors.Errorf("ACL entry at offset %d is truncated", pos)
			}
			id = binary.LittleEndian.Uint32(disk[pos+4:])
			pos += aclLongEntrySize
		default:
			return nil, xerrors.Errorf("unknown ACL tag 0x%x at offset %d", tag, pos)
		}

		var e [aclLongEntrySize]byte
		binary.LittleEndian.PutUint16(e[0:], tag)
		binary.LittleEndian.PutUint16(e[2:], perm)
		binary.LittleEndian.PutUint32(e[4:], id)
		out = append(out, e[:]...)
	}
	return out, nil
}
