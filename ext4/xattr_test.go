package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
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

// assertParseError runs parseXattrEntries and checks the error text. It
// returns the error so callers can inspect it further.
func assertParseError(t *testing.T, region []byte, entriesBase, storageSize int,
	eaInode bool, readEA eaValueReader, wantSubstr string) error {
	t.Helper()
	out := make(map[string][]byte)
	err := parseXattrEntries(region, entriesBase, storageSize, eaInode, readEA, out)
	if err == nil {
		t.Fatalf("parseXattrEntries succeeded with %v, want an error containing %q", out, wantSubstr)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("parseXattrEntries error = %q, want it to contain %q", err, wantSubstr)
	}
	return err
}

// eaCall records one call to a fake eaValueReader.
type eaCall struct {
	inum uint32
	size int
}

// fakeEAReader returns a reader that answers from values, keyed by inode
// number, and records every call. Each call returns a fresh copy.
func fakeEAReader(values map[uint32][]byte, calls *[]eaCall) eaValueReader {
	return func(inum uint32, size int) ([]byte, error) {
		*calls = append(*calls, eaCall{inum, size})
		v, ok := values[inum]
		if !ok {
			return nil, errors.New("fake: no such EA inode")
		}
		return append([]byte(nil), v...), nil
	}
}

// pattern returns n bytes of a repeating, position-dependent pattern, so a
// truncated or shifted value does not compare equal.
func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) ^ seed
	}
	return b
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
	if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
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
	if err := parseXattrEntries(region, base, len(region)-base, false, nil, out); err != nil {
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
	if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
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
	if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{"user.kept": []byte("v")})

	// Garbage after the terminator that would fail the walk is ignored too.
	// Bytes end+4 .. end+20 are the 16-byte entry header just past the terminator word.
	for i := end + 4; i < end+20; i++ {
		region[i] = 0xff
	}
	out = make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
		t.Fatalf("parseXattrEntries with garbage after the terminator: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{"user.kept": []byte("v")})
}

func TestParseXattrErrors(t *testing.T) {
	t.Run("name length overruns storage", func(t *testing.T) {
		region := buildXattrRegion(0, 32, testEntry{index: 1, name: "a"})
		region[0] = 200 // e_name_len
		assertParseError(t, region, 0, len(region), false, nil, "xattr name length 200 overruns storage")
	})

	t.Run("value size exceeds remaining storage", func(t *testing.T) {
		// 64 bytes of storage less a 20-byte entry leaves 44.
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 24, size: 45})
		assertParseError(t, region, 0, len(region), false, nil,
			`xattr "user.a" value size 45 exceeds remaining storage 44`)
	})

	t.Run("value size over 64 KiB", func(t *testing.T) {
		region := buildXattrRegion(0, 70000,
			testEntry{index: 1, name: "a", offs: 24, size: xattrValueMax + 1})
		assertParseError(t, region, 0, len(region), false, nil,
			`xattr "user.a" value size 65537 exceeds maximum 65536`)
	})

	t.Run("value offset plus size past the region", func(t *testing.T) {
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 60, size: 8})
		assertParseError(t, region, 0, len(region), false, nil,
			`xattr "user.a" value at 60+8 overruns region 64`)
	})

	t.Run("value overlaps the entry table", func(t *testing.T) {
		// The table ends at 20; values may start at 24, past the terminator.
		// No value bytes are written, so the terminator stays intact.
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 20, size: 4})
		assertParseError(t, region, 0, len(region), false, nil,
			`xattr "user.a" value offset 20 overlaps the entry table`)
	})

	t.Run("value overlaps the block header", func(t *testing.T) {
		// Block case: an offset inside the header is also before the table end.
		region := buildXattrRegion(xattrHeaderSize, 64, testEntry{index: 1, name: "a", offs: 4, size: 4})
		assertParseError(t, region, xattrHeaderSize, 64, false, nil,
			`xattr "user.a" value offset 4 overlaps the entry table`)
	})

	t.Run("storage size overruns the region", func(t *testing.T) {
		region := buildXattrRegion(xattrHeaderSize, 32)
		assertParseError(t, region, xattrHeaderSize, 64, false, nil,
			"xattr storage size 96 overruns region 64")
	})
}

// Accept-side boundaries of the inline value checks: each limit is inclusive.
func TestParseXattrBoundaries(t *testing.T) {
	t.Run("value offset exactly past the terminator", func(t *testing.T) {
		// The table ends at 20 (one entry with a 1-byte name); 24 is the first
		// offset past the 4-byte terminator.
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "a", offs: 24, value: []byte("abcd")})
		out := make(map[string][]byte)
		if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
			t.Fatalf("parseXattrEntries: %v", err)
		}
		assertXattrs(t, out, map[string][]byte{"user.a": []byte("abcd")})
	})

	t.Run("block value offset exactly past the terminator", func(t *testing.T) {
		// Block case: the table ends at 32+20, so 32+24 is the first legal offset.
		const base = xattrHeaderSize
		region := buildXattrRegion(base, 64, testEntry{index: 1, name: "a", offs: base + 24, value: []byte("abcd")})
		out := make(map[string][]byte)
		if err := parseXattrEntries(region, base, 64, false, nil, out); err != nil {
			t.Fatalf("parseXattrEntries: %v", err)
		}
		assertXattrs(t, out, map[string][]byte{"user.a": []byte("abcd")})
	})

	t.Run("value size equals remaining storage", func(t *testing.T) {
		// remain does not count the terminator, so a value can only use all of
		// it when storageSize is smaller than the region. Storage 64 less a
		// 20-byte entry leaves exactly 44; the value sits at 24..68.
		value := bytes.Repeat([]byte{'v'}, 44)
		region := buildXattrRegion(0, 68, testEntry{index: 1, name: "a", offs: 24, value: value})
		out := make(map[string][]byte)
		if err := parseXattrEntries(region, 0, 64, false, nil, out); err != nil {
			t.Fatalf("parseXattrEntries: %v", err)
		}
		assertXattrs(t, out, map[string][]byte{"user.a": value})
	})

	t.Run("value of exactly 64 KiB", func(t *testing.T) {
		value := bytes.Repeat([]byte{0x5a}, xattrValueMax)
		region := buildXattrRegion(0, 24+xattrValueMax, testEntry{index: 1, name: "a", offs: 24, value: value})
		out := make(map[string][]byte)
		if err := parseXattrEntries(region, 0, len(region), false, nil, out); err != nil {
			t.Fatalf("parseXattrEntries: %v", err)
		}
		assertXattrs(t, out, map[string][]byte{"user.a": value})
	})
}

// A value stored in another inode is refused when the superblock does not
// declare ea_inode.
func TestParseXattrValueInodeWithoutFeature(t *testing.T) {
	region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100000, inum: 12})
	assertParseError(t, region, 0, len(region), false, nil,
		`xattr "user.big" has value inode 12 but ea_inode feature is not set`)
}

// A value stored in an EA inode is fetched through the reader, which is told
// the inode number and the size the entry declares.
func TestParseXattrValueInode(t *testing.T) {
	want := pattern(8192, 0x3c)
	var calls []eaCall
	readEA := fakeEAReader(map[uint32][]byte{12: want}, &calls)
	region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 8192, inum: 12})

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), true, readEA, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{"user.big": want})
	if len(calls) != 1 || calls[0] != (eaCall{12, 8192}) {
		t.Errorf("reader calls = %v, want [{12 8192}]", calls)
	}
}

// An EA-inode value does not use the storage area: its size is not taken from
// the remaining budget and its offset is not read. A 64 KiB EA-inode value
// sits beside two inline values in a 256-byte region, and all three come
// back.
func TestParseXattrValueInodeBesideInline(t *testing.T) {
	big := pattern(xattrValueMax, 0xa5)
	var calls []eaCall
	readEA := fakeEAReader(map[uint32][]byte{40: big}, &calls)
	region := buildXattrRegion(0, 256,
		testEntry{index: 1, name: "small1", value: []byte("first")},
		// offs 1 lies inside the entry table, which an inline value would be
		// refused for; an EA-inode entry's offset must be ignored.
		testEntry{index: 1, name: "big", offs: 1, size: xattrValueMax, inum: 40},
		testEntry{index: 6, name: "small2", value: []byte("second value")},
	)

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), true, readEA, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{
		"user.small1":     []byte("first"),
		"user.big":        big,
		"security.small2": []byte("second value"),
	})
	if len(calls) != 1 || calls[0] != (eaCall{40, xattrValueMax}) {
		t.Errorf("reader calls = %v, want [{40 %d}]", calls, xattrValueMax)
	}
}

// Two entries may name the same EA inode (ext4 shares EA inodes between
// identical values). Both are returned.
func TestParseXattrSharedValueInode(t *testing.T) {
	want := pattern(300, 0x11)
	var calls []eaCall
	readEA := fakeEAReader(map[uint32][]byte{77: want}, &calls)
	region := buildXattrRegion(0, 96,
		testEntry{index: 1, name: "a", size: 300, inum: 77},
		testEntry{index: 4, name: "b", size: 300, inum: 77},
	)

	out := make(map[string][]byte)
	if err := parseXattrEntries(region, 0, len(region), true, readEA, out); err != nil {
		t.Fatalf("parseXattrEntries: %v", err)
	}
	assertXattrs(t, out, map[string][]byte{
		"user.a":    want,
		"trusted.b": want,
	})
	if len(calls) != 2 {
		t.Errorf("reader calls = %v, want two", calls)
	}
}

func TestParseXattrValueInodeErrors(t *testing.T) {
	// failIfCalled is a reader that must not be reached.
	failIfCalled := func(t *testing.T) eaValueReader {
		return func(inum uint32, size int) ([]byte, error) {
			t.Errorf("reader called with inode %d size %d", inum, size)
			return make([]byte, size), nil
		}
	}

	t.Run("size 0", func(t *testing.T) {
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", inum: 12})
		assertParseError(t, region, 0, len(region), true, failIfCalled(t),
			`xattr "user.big" in inode 12 has value size 0`)
	})

	t.Run("size over 64 KiB", func(t *testing.T) {
		region := buildXattrRegion(0, 64,
			testEntry{index: 1, name: "big", size: xattrValueMax + 1, inum: 12})
		assertParseError(t, region, 0, len(region), true, failIfCalled(t),
			`xattr "user.big" value size 65537 exceeds maximum 65536`)
	})

	t.Run("reader error", func(t *testing.T) {
		sentinel := errors.New("disk on fire")
		readEA := func(uint32, int) ([]byte, error) { return nil, sentinel }
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100, inum: 12})
		err := assertParseError(t, region, 0, len(region), true, readEA,
			`xattr "user.big" is stored in inode 12: disk on fire`)
		if !errors.Is(err, sentinel) {
			t.Errorf("error %v does not wrap the reader's error", err)
		}
	})

	t.Run("reader returns a short value", func(t *testing.T) {
		readEA := func(uint32, int) ([]byte, error) { return make([]byte, 99), nil }
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100, inum: 12})
		assertParseError(t, region, 0, len(region), true, readEA,
			`xattr "user.big": inode 12 returned 99 bytes, want 100`)
	})

	t.Run("reader returns a long value", func(t *testing.T) {
		readEA := func(uint32, int) ([]byte, error) { return make([]byte, 101), nil }
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100, inum: 12})
		assertParseError(t, region, 0, len(region), true, readEA,
			`xattr "user.big": inode 12 returned 101 bytes, want 100`)
	})

	t.Run("nil reader", func(t *testing.T) {
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100, inum: 12})
		assertParseError(t, region, 0, len(region), true, nil,
			`xattr "user.big" is stored in inode 12 but no EA inode reader was given`)
	})

	t.Run("feature off wins over the reader", func(t *testing.T) {
		region := buildXattrRegion(0, 64, testEntry{index: 1, name: "big", size: 100, inum: 12})
		assertParseError(t, region, 0, len(region), false, failIfCalled(t),
			`xattr "user.big" has value inode 12 but ea_inode feature is not set`)
	})
}

// readEAInodeValue refuses what cannot be an EA inode before reading
// anything: a reserved or out-of-range inode number, and a size the parser
// would have refused. The FileSystem here has a superblock only, so any read
// would fail with a different error.
func TestReadEAInodeValueRange(t *testing.T) {
	cases := []struct {
		name     string
		rev      uint32
		firstIno uint32
		inum     uint32
		size     int
		want     string
	}{
		{"inode 0", 1, 11, 0, 100, "EA inode 0 out of range 11..100"},
		{"below s_first_ino", 1, 20, 19, 100, "EA inode 19 out of range 20..100"},
		{"revision 0 has no s_first_ino", 0, 50, 10, 100, "EA inode 10 out of range 11..100"},
		{"past the inode count", 1, 11, 101, 100, "EA inode 101 out of range 11..100"},
		{"size 0", 1, 11, 12, 0, "EA inode 12: value size 0 is invalid"},
		{"size over 64 KiB", 1, 11, 12, xattrValueMax + 1, "EA inode 12: value size 65537 is invalid"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := &FileSystem{sb: Superblock{RevLevel: c.rev, FirstIno: c.firstIno, InodeCount: 100}}
			v, err := fsys.readEAInodeValue(c.inum, c.size)
			if err == nil || err.Error() != c.want {
				t.Errorf("readEAInodeValue(%d, %d) = %d bytes, %v; want %q", c.inum, c.size, len(v), err, c.want)
			}
		})
	}
}

// s_first_ino below 11 would make reserved inodes (the root directory, the
// journal, the resize inode) eligible as EA inodes. The kernel refuses to
// mount such a filesystem; the reader keeps 11 as the floor.
func TestReadEAInodeValueFirstInoFloor(t *testing.T) {
	cases := []struct {
		name     string
		firstIno uint32
		inum     uint32
		want     string
	}{
		{"s_first_ino 2, inode 5", 2, 5, "EA inode 5 out of range 11..100"},
		{"s_first_ino 2, inode 2", 2, 2, "EA inode 2 out of range 11..100"},
		{"s_first_ino 0, inode 10", 0, 10, "EA inode 10 out of range 11..100"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := &FileSystem{sb: Superblock{RevLevel: 1, FirstIno: c.firstIno, InodeCount: 100}}
			v, err := fsys.readEAInodeValue(c.inum, 100)
			if err == nil || err.Error() != c.want {
				t.Errorf("readEAInodeValue(%d, 100) = %d bytes, %v; want %q", c.inum, len(v), err, c.want)
			}
		})
	}
}

// eaExtentInode returns an EA inode whose i_block holds a depth-0 extent
// header and the given extents.
func eaExtentInode(extents ...Extent) *Inode {
	inode := &Inode{Mode: FileTypeRegular | 0o600, Flags: EXTENTS_FL | EA_INODE_FL}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, &ExtentHeader{Magic: 0xF30A, Entries: uint16(len(extents)), Max: 4})
	for i := range extents {
		binary.Write(&buf, binary.LittleEndian, &extents[i])
	}
	copy(inode.BlockOrExtents[:], buf.Bytes())
	return inode
}

// eaInodeBlocks maps the logical blocks of an extent-mapped EA inode. A
// logical block claimed by two extents, or lying in an unwritten extent, is
// damage: there is no telling which bytes are the value.
func TestEAInodeBlocksExtents(t *testing.T) {
	const unwritten = extentInitMaxLen // Len above this marks an unwritten extent
	fsys := &FileSystem{sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount}}

	t.Run("adjacent extents", func(t *testing.T) {
		inode := eaExtentInode(
			Extent{Block: 0, Len: 2, StartLo: 10},
			Extent{Block: 2, Len: 1, StartLo: 30},
			Extent{Block: 5, Len: 1, StartLo: 40}, // past nblocks: not looked at
		)
		got, err := fsys.eaInodeBlocks(inode, 3)
		if err != nil {
			t.Fatalf("eaInodeBlocks: %v", err)
		}
		if want := []int64{10, 11, 30}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("eaInodeBlocks = %v, want %v", got, want)
		}
	})

	cases := []struct {
		name    string
		inode   *Inode
		nblocks int64
		want    string
	}{
		{"logical block mapped twice", eaExtentInode(
			Extent{Block: 0, Len: 2, StartLo: 10},
			Extent{Block: 1, Len: 2, StartLo: 20},
		), 3, "logical block 1 is mapped twice"},
		{"same extent twice", eaExtentInode(
			Extent{Block: 0, Len: 1, StartLo: 10},
			Extent{Block: 0, Len: 1, StartLo: 10},
		), 1, "logical block 0 is mapped twice"},
		{"unwritten extent", eaExtentInode(
			Extent{Block: 0, Len: 1, StartLo: 10},
			Extent{Block: 1, Len: unwritten + 1, StartLo: 20},
		), 2, "logical block 1 is in an unwritten extent"},
		{"unwritten first extent", eaExtentInode(
			Extent{Block: 0, Len: unwritten + 2, StartLo: 10},
		), 2, "logical block 0 is in an unwritten extent"},
		// 0 is what eaInodeBlocks returns for an unmapped block, so an
		// extent pointing at physical block 0 is refused by name here
		// rather than reported later as a hole.
		{"extent at physical block 0", eaExtentInode(
			Extent{Block: 0, Len: 1, StartLo: 0},
		), 1, "logical block 0 maps to physical block 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := fsys.eaInodeBlocks(c.inode, c.nblocks)
			if err == nil || err.Error() != c.want {
				t.Errorf("eaInodeBlocks = %v, %v; want %q", got, err, c.want)
			}
		})
	}
}
