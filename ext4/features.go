package ext4

import (
	"strconv"
	"strings"

	"golang.org/x/xerrors"
)

// Feature bits not already declared in const.go, needed so that an
// unsupported filesystem can be named rather than reported as a bare hex
// mask.
const (
	FEATURE_COMPAT_FAST_COMMIT   = 0x0400
	FEATURE_COMPAT_STABLE_INODES = 0x0800
	FEATURE_COMPAT_ORPHAN_FILE   = 0x1000

	FEATURE_RO_COMPAT_HAS_SNAPSHOT   = 0x0080
	FEATURE_RO_COMPAT_REPLICA        = 0x0800
	FEATURE_RO_COMPAT_SHARED_BLOCKS  = 0x4000
	FEATURE_RO_COMPAT_VERITY         = 0x8000
	FEATURE_RO_COMPAT_ORPHAN_PRESENT = 0x10000

	FEATURE_INCOMPAT_CASEFOLD = 0x20000
)

// The feature set this reader understands.
//
// These match what mke2fs produces for the skeleton images, pinned
// explicitly on the writer side rather than inherited from
// /etc/mke2fs.conf:
//
//	sparse_super large_file filetype dir_index ext_attr extent
//	huge_file flex_bg metadata_csum 64bit dir_nlink extra_isize
//	resize_inode
const (
	// COMPAT features are safe to ignore entirely -- that is what the
	// category means. Listed for reporting only; CheckSupported does not
	// fail on unknown COMPAT bits.
	supportedCompat = FEATURE_COMPAT_EXT_ATTR |
		FEATURE_COMPAT_RESIZE_INODE |
		FEATURE_COMPAT_DIR_INDEX |
		FEATURE_COMPAT_HAS_JOURNAL // read-only never replays, see below

	// RO_COMPAT features are, by the ext4 contract, safe for a read-only
	// consumer to ignore. We still allowlist them, because BIGALLOC is a
	// known exception: it decouples cluster size from block size, and a
	// reader that assumes they are equal miscomputes.
	supportedRoCompat = FEATURE_RO_COMPAT_SPARSE_SUPER |
		FEATURE_RO_COMPAT_LARGE_FILE |
		FEATURE_RO_COMPAT_HUGE_FILE |
		FEATURE_RO_COMPAT_DIR_NLINK |
		FEATURE_RO_COMPAT_EXTRA_ISIZE |
		FEATURE_RO_COMPAT_METADATA_CSUM |
		FEATURE_RO_COMPAT_GDT_CSUM

	// INCOMPAT features MUST be understood. An unknown bit here means the
	// on-disk layout is something this reader cannot interpret, and
	// carrying on would return wrong bytes rather than an error.
	supportedIncompat = FEATURE_INCOMPAT_FILETYPE |
		FEATURE_INCOMPAT_EXTENTS |
		FEATURE_INCOMPAT_64BIT |
		FEATURE_INCOMPAT_FLEX_BG
)

type featureName struct {
	bit  uint32
	name string
}

var compatNames = []featureName{
	{FEATURE_COMPAT_DIR_PREALLOC, "dir_prealloc"},
	{FEATURE_COMPAT_IMAGIC_INODES, "imagic_inodes"},
	{FEATURE_COMPAT_HAS_JOURNAL, "has_journal"},
	{FEATURE_COMPAT_EXT_ATTR, "ext_attr"},
	{FEATURE_COMPAT_RESIZE_INODE, "resize_inode"},
	{FEATURE_COMPAT_DIR_INDEX, "dir_index"},
	{FEATURE_COMPAT_SPARSE_SUPER2, "sparse_super2"},
	{FEATURE_COMPAT_FAST_COMMIT, "fast_commit"},
	{FEATURE_COMPAT_STABLE_INODES, "stable_inodes"},
	{FEATURE_COMPAT_ORPHAN_FILE, "orphan_file"},
}

var roCompatNames = []featureName{
	{FEATURE_RO_COMPAT_SPARSE_SUPER, "sparse_super"},
	{FEATURE_RO_COMPAT_LARGE_FILE, "large_file"},
	{FEATURE_RO_COMPAT_BTREE_DIR, "btree_dir"},
	{FEATURE_RO_COMPAT_HUGE_FILE, "huge_file"},
	{FEATURE_RO_COMPAT_GDT_CSUM, "uninit_bg"},
	{FEATURE_RO_COMPAT_DIR_NLINK, "dir_nlink"},
	{FEATURE_RO_COMPAT_EXTRA_ISIZE, "extra_isize"},
	{FEATURE_RO_COMPAT_HAS_SNAPSHOT, "snapshot"},
	{FEATURE_RO_COMPAT_QUOTA, "quota"},
	{FEATURE_RO_COMPAT_BIGALLOC, "bigalloc"},
	{FEATURE_RO_COMPAT_METADATA_CSUM, "metadata_csum"},
	{FEATURE_RO_COMPAT_REPLICA, "replica"},
	{FEATURE_RO_COMPAT_READONLY, "read-only"},
	{FEATURE_RO_COMPAT_PROJECT, "project"},
	{FEATURE_RO_COMPAT_SHARED_BLOCKS, "shared_blocks"},
	{FEATURE_RO_COMPAT_VERITY, "verity"},
	{FEATURE_RO_COMPAT_ORPHAN_PRESENT, "orphan_present"},
}

var incompatNames = []featureName{
	{FEATURE_INCOMPAT_COMPRESSION, "compression"},
	{FEATURE_INCOMPAT_FILETYPE, "filetype"},
	{FEATURE_INCOMPAT_RECOVER, "needs_recovery"},
	{FEATURE_INCOMPAT_JOURNAL_DEV, "journal_dev"},
	{FEATURE_INCOMPAT_META_BG, "meta_bg"},
	{FEATURE_INCOMPAT_EXTENTS, "extent"},
	{FEATURE_INCOMPAT_64BIT, "64bit"},
	{FEATURE_INCOMPAT_MMP, "mmp"},
	{FEATURE_INCOMPAT_FLEX_BG, "flex_bg"},
	{FEATURE_INCOMPAT_EA_INODE, "ea_inode"},
	{FEATURE_INCOMPAT_DIRDATA, "dirdata"},
	{FEATURE_INCOMPAT_CSUM_SEED, "metadata_csum_seed"},
	{FEATURE_INCOMPAT_LARGEDIR, "largedir"},
	{FEATURE_INCOMPAT_INLINE_DATA, "inline_data"},
	{FEATURE_INCOMPAT_ENCRYPT, "encrypt"},
	{FEATURE_INCOMPAT_CASEFOLD, "casefold"},
}

// describeFeatures renders a feature mask as a space-separated list of names,
// with any bit that has no name shown as 0x<hex>.
func describeFeatures(mask uint32, names []featureName) string {
	var out []string
	remaining := mask
	for _, f := range names {
		if mask&f.bit != 0 {
			out = append(out, f.name)
			remaining &^= f.bit
		}
	}
	if remaining != 0 {
		out = append(out, "0x"+strconv.FormatUint(uint64(remaining), 16))
	}
	return strings.Join(out, " ")
}

// UnsupportedCompatFeatures returns the COMPAT bits this reader does not know
// about. They are safe to ignore -- that is what the COMPAT category means --
// so CheckSupported does not fail on them. Exposed so a caller that wants to
// log drift can.
func (sb *Superblock) UnsupportedCompatFeatures() uint32 {
	return sb.FeatureCompat &^ uint32(supportedCompat)
}

// CheckSupported reports whether this reader can interpret the filesystem.
//
// It fails on any INCOMPAT or RO_COMPAT feature outside the supported set.
// The point is to turn "restored the wrong bytes silently" into a clean
// error: without this check, a filesystem using a feature the reader does not
// implement parses successfully and produces corrupt output.
//
// Notable rejections, all of which would otherwise be silent:
//
//   - needs_recovery: the journal holds unreplayed changes. A read-only
//     consumer never replays it, so reads would return stale data.
//   - bigalloc: allocation is in clusters, not blocks.
//   - inline_data: small file contents live in the inode body.
//   - encrypt / casefold / verity: content or names are not what they appear.
//   - ea_inode: extended attribute values live in separate inodes.
//   - meta_bg: group descriptors are laid out differently.
//
// has_journal itself is accepted. It is a COMPAT feature and a clean journal
// needs no replay; a dirty one is caught by needs_recovery above.
func (sb *Superblock) CheckSupported() error {
	badIncompat := sb.FeatureIncompat &^ uint32(supportedIncompat)
	badRoCompat := sb.FeatureRoCompat &^ uint32(supportedRoCompat)

	if badIncompat == 0 && badRoCompat == 0 {
		return nil
	}

	var parts []string
	if badIncompat != 0 {
		parts = append(parts, "incompat: "+describeFeatures(badIncompat, incompatNames))
	}
	if badRoCompat != 0 {
		parts = append(parts, "ro_compat: "+describeFeatures(badRoCompat, roCompatNames))
	}
	return xerrors.Errorf("unsupported ext4 features -- %s", strings.Join(parts, "; "))
}
