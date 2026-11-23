package ext4

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// testEntry describes one ext2_ext_attr_entry for buildXattrRegion.
type testEntry struct {
	index byte   // e_name_index
	name  string // name suffix, stored after the entry header
	value []byte // value bytes, copied to the value offset when they fit

	// offs is e_value_offs. Zero means place the value automatically: values
	// are packed downwards from the end of the storage area, 4-byte aligned,
	// the way ext4 lays them out.
	offs int
	// size is e_value_size. Zero means len(value).
	size uint32
	// inum is e_value_inum.
	inum uint32
}

// buildXattrRegion returns a storage region of entriesBase+size bytes with the
// entry table at entriesBase, followed by a zero terminator word (the rest of
// the region is zero). Value offsets are relative to the region start, which
// is what parseXattrEntries expects in both the inline (entriesBase 0) and the
// block (entriesBase 32) case. The bytes before entriesBase are left zero:
// parseXattrEntries does not look at the magic or the block header.
func buildXattrRegion(entriesBase int, size int, entries ...testEntry) []byte {
	region := make([]byte, entriesBase+size)
	valueEnd := len(region)
	pos := entriesBase
	for _, e := range entries {
		valueSize := e.size
		if valueSize == 0 {
			valueSize = uint32(len(e.value))
		}
		offs := e.offs
		if offs == 0 && len(e.value) > 0 {
			valueEnd -= (len(e.value) + 3) & ^3
			offs = valueEnd
		}
		region[pos] = byte(len(e.name))
		region[pos+1] = e.index
		binary.LittleEndian.PutUint16(region[pos+2:], uint16(offs))
		binary.LittleEndian.PutUint32(region[pos+4:], e.inum)
		binary.LittleEndian.PutUint32(region[pos+8:], valueSize)
		copy(region[pos+xattrEntrySize:], e.name)
		if offs+len(e.value) <= len(region) {
			copy(region[offs:], e.value)
		}
		pos += entryLen(len(e.name))
	}
	return region
}

// assertXattrs checks that got holds exactly want.
func assertXattrs(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got %d attributes %v, want %d", len(got), got, len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("attribute %q missing", name)
			continue
		}
		if !bytes.Equal(g, w) {
			t.Errorf("attribute %q = %q, want %q", name, g, w)
		}
	}
}

// assertParseError runs parseXattrEntries and checks the error text.
func assertParseError(t *testing.T, region []byte, entriesBase, storageSize int,
	eaInode bool, wantSubstr string) {
	t.Helper()
	out := make(map[string][]byte)
	err := parseXattrEntries(region, entriesBase, storageSize, eaInode, out)
	if err == nil {
		t.Fatalf("parseXattrEntries succeeded with %v, want an error containing %q", out, wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("parseXattrEntries error = %q, want it to contain %q", err, wantSubstr)
	}
}

// Inline storage: the entry table starts at offset 0 of the region and value
// offsets are measured from the same point. Name indexes are expanded to
// their prefixes, and index 2 is a complete name with an empty suffix.
func TestParseXattrInline(t *testing.T) {
	acl := []byte{1, 0, 0, 0, 1, 0, 6, 0} // raw bytes; the parser does not convert ACLs
	region := buildXattrRegion(0, 160,
		testEntry{index: 1, name: "foo", value: []byte("bar")},
		testEntry{index: 6, name: "selinux", value: []byte("system_u:object_r:etc_t:s0\x00")},
		testEntry{index: 2, name: "", value: acl},
	)

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), false, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{
		"user.foo":                []byte("bar"),
		"security.selinux":        []byte("system_u:object_r:etc_t:s0\x00"),
		"system.posix_acl_access": acl,
	})
}

// Block storage: entries start after the 32-byte header, but value offsets
// are measured from the start of the block.
func TestParseXattrBlock(t *testing.T) {
	const base = xattrHeaderSize
	region := buildXattrRegion(base, 224,
		testEntry{index: 1, name: "one", value: []byte("first value")},
		testEntry{index: 4, name: "two", value: []byte("second")},
	)
	// Sanity check on the fixture: the first value sits at the block end.
	if got := int(binary.LittleEndian.Uint16(region[base+2:])); got != 256-12 {
		t.Fatalf("fixture: first value offset = %d, want %d", got, 256-12)
	}

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, base, len(region)-base, false, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{
		"user.one":    []byte("first value"),
		"trusted.two": []byte("second"),
	})
}

// A zero-length value comes back as a present, empty, non-nil slice. Its
// offset is not checked against the entry table.
func TestParseXattrEmptyValue(t *testing.T) {
	region := buildXattrRegion(0, 64,
		testEntry{index: 1, name: "empty"},
		testEntry{index: 1, name: "full", value: []byte("x")},
	)

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), false, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{
		"user.empty": {},
		"user.full":  []byte("x"),
	})
	if v, ok := out["user.empty"]; !ok || v == nil {
		t.Errorf("user.empty = %#v, want a non-nil empty slice", v)
	}
}

// A zero word ends the entry table: anything after it is ignored, even bytes
// that look like a valid entry.
func TestParseXattrTerminator(t *testing.T) {
	region := buildXattrRegion(0, 96,
		testEntry{index: 1, name: "kept", value: []byte("v")},
	)
	// Write a well-formed entry just past the terminator word.
	ghost := buildXattrRegion(0, 32, testEntry{index: 1, name: "ghost", offs: 80, size: 1})
	end := entryLen(len("kept"))
	copy(region[end+4:], ghost[:entryLen(len("ghost"))])

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), false, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{"user.kept": []byte("v")})

	// Garbage after the terminator that would fail the walk is ignored too.
	for i := end + 4; i < end+20; i++ {
		region[i] = 0xff
	}
	out = make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), false, out); err != nil {
		t.Fatalf("parseXattrEntries with garbage after the terminator: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{"user.kept": []byte("v")})
}

func TestParseXattrErrors(t *testing.T) {
	t.Run("name length overruns storage", func(t *testing.T) {
		region := buildXattrRegion(0, 32, testEntry{index: 1, name: "a"})
		region[0] = 200 // e_name_len
		assertParseError(t, region, 0, len(region), false, "xattr name length 200 overruns storage")
	})

	t.Run("value size exceeds remaining storage", func(t *testing.T) {
		// 64 bytes of storage less a 20-byte entry leaves 44.
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 24, size: 45})
		assertParseError(t, region, 0, len(region), false,
			`xattr "user.a" value size 45 exceeds remaining storage 44`)
	})

	t.Run("value size over 64 KiB", func(t *testing.T) {
		region := buildXattrRegion(0, 70000,
			testEntry{index: 1, name: "a", offs: 24, size: xattrValueMax + 1})
		assertParseError(t, region, 0, len(region), false,
			`xattr "user.a" value size 65537 exceeds maximum 65536`)
	})

	t.Run("value offset plus size past the region", func(t *testing.T) {
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 60, size: 8})
		assertParseError(t, region, 0, len(region), false,
			`xattr "user.a" value at 60+8 overruns region 64`)
	})

	t.Run("value overlaps the entry table", func(t *testing.T) {
		// The table ends at 20; values may start at 24, past the terminator.
		// No value bytes are written, so the terminator stays intact.
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 20, size: 4})
		assertParseError(t, region, 0, len(region), false,
			`xattr "user.a" value offset 20 overlaps the entry table`)
	})

	t.Run("value overlaps the block header", func(t *testing.T) {
		// Block case: an offset inside the header is also before the table end.
		region := buildXattrRegion(xattrHeaderSize, 64, testEntry{index: 1, name: "a", offs: 4, size: 4})
		assertParseError(t, region, xattrHeaderSize, 64, false,
			`xattr "user.a" value offset 4 overlaps the entry table`)
	})

	t.Run("storage size overruns the region", func(t *testing.T) {
		region := buildXattrRegion(xattrHeaderSize, 32)
		assertParseError(t, region, xattrHeaderSize, 64, false,
			"xattr storage size 96 overruns region 64")
	})
}

// A value stored in another inode is refused when the superblock does not
// declare ea_inode.
func TestParseXattrValueInodeWithoutFeature(t *testing.T) {
	region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100000, inum: 12})
	assertParseError(t, region, 0, len(region), false,
		`xattr "user.big" has value inode 12 but ea_inode feature is not set`)
}

// With ea_inode declared, a value stored in another inode is still refused:
// reading EA inodes is not implemented yet.
func TestParseXattrValueInodeUnsupported(t *testing.T) {
	region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100000, inum: 12})
	assertParseError(t, region, 0, len(region), true,
		`xattr "user.big" is stored in inode 12: ea_inode is not supported`)
}
