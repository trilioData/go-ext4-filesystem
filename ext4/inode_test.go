package ext4

import "testing"

func TestInodeFileType(t *testing.T) {
	tests := []struct {
		name            string
		mode            uint16
		wantDir         bool
		wantRegular     bool
		wantSymlink     bool
		wantSocket      bool
		wantFifo        bool
		wantCharDevice  bool
		wantBlockDevice bool
	}{
		{
			name:        "regular file (0100644)",
			mode:        0100644,
			wantRegular: true,
		},
		{
			name:    "directory (040755)",
			mode:    040755,
			wantDir: true,
		},
		{
			name:        "symlink (0120777)",
			mode:        0120777,
			wantSymlink: true,
		},
		{
			name:       "socket (0140755)",
			mode:       0140755,
			wantSocket: true,
		},
		{
			name:     "fifo (010644)",
			mode:     010644,
			wantFifo: true,
		},
		{
			name:           "char device (020666)",
			mode:           020666,
			wantCharDevice: true,
		},
		{
			name:            "block device (060660)",
			mode:            060660,
			wantBlockDevice: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inode := Inode{Mode: tt.mode}
			if got := inode.IsDir(); got != tt.wantDir {
				t.Errorf("IsDir() = %v, want %v (mode: %#o)", got, tt.wantDir, tt.mode)
			}
			if got := inode.IsRegular(); got != tt.wantRegular {
				t.Errorf("IsRegular() = %v, want %v (mode: %#o)", got, tt.wantRegular, tt.mode)
			}
			if got := inode.IsSymlink(); got != tt.wantSymlink {
				t.Errorf("IsSymlink() = %v, want %v (mode: %#o)", got, tt.wantSymlink, tt.mode)
			}
			if got := inode.IsSocket(); got != tt.wantSocket {
				t.Errorf("IsSocket() = %v, want %v (mode: %#o)", got, tt.wantSocket, tt.mode)
			}
			if got := inode.IsFifo(); got != tt.wantFifo {
				t.Errorf("IsFifo() = %v, want %v (mode: %#o)", got, tt.wantFifo, tt.mode)
			}
			if got := inode.IsCharDevice(); got != tt.wantCharDevice {
				t.Errorf("IsCharDevice() = %v, want %v (mode: %#o)", got, tt.wantCharDevice, tt.mode)
			}
			if got := inode.IsBlockDevice(); got != tt.wantBlockDevice {
				t.Errorf("IsBlockDevice() = %v, want %v (mode: %#o)", got, tt.wantBlockDevice, tt.mode)
			}
		})
	}
}

func TestExtent_IsUninitialized(t *testing.T) {
	tests := []struct {
		name   string
		len    uint16
		wantUn bool
		wantN  uint16
	}{
		{"initialized: 1 block", 1, false, 1},
		{"initialized: max (0x7FFF)", 0x7FFF, false, 0x7FFF},
		{"uninitialized: 1 block (0x8001)", 0x8001, true, 1},
		{"uninitialized: max (0xFFFF)", 0xFFFF, true, 0x7FFF},
		{"zero length", 0, false, 0},
		// 0x8000 is EXT_INIT_MAX_LEN: a *written* extent of the maximum
		// length ext4 can describe, not an unwritten one. The boundary is a
		// comparison against 32768, not a test of bit 15, and this is the one
		// value where those two disagree. See TestExtentLenBoundary.
		{"exactly 0x8000 (written, max length)", 0x8000, false, 0x8000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Extent{Len: tt.len}
			if got := e.IsUninitialized(); got != tt.wantUn {
				t.Errorf("IsUninitialized() = %v, want %v", got, tt.wantUn)
			}
			if got := e.GetLen(); got != tt.wantN {
				t.Errorf("GetLen() = %d, want %d", got, tt.wantN)
			}
		})
	}
}

func TestInodeFileTypeMutualExclusion(t *testing.T) {
	modes := []struct {
		name string
		mode uint16
	}{
		{"regular", 0100644},
		{"directory", 040755},
		{"symlink", 0120777},
		{"socket", 0140755},
		{"fifo", 010644},
		{"char device", 020666},
		{"block device", 060660},
	}

	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			inode := Inode{Mode: m.mode}
			count := 0
			if inode.IsDir() {
				count++
			}
			if inode.IsRegular() {
				count++
			}
			if inode.IsSymlink() {
				count++
			}
			if inode.IsSocket() {
				count++
			}
			if inode.IsFifo() {
				count++
			}
			if inode.IsCharDevice() {
				count++
			}
			if inode.IsBlockDevice() {
				count++
			}
			if count != 1 {
				t.Errorf("expected exactly 1 type match for mode %#o, got %d", m.mode, count)
			}
		})
	}
}

// TestExtentLenBoundary pins the written/unwritten boundary.
//
// ext4 packs a block count and the unwritten flag into one 16-bit field by
// splitting its range at EXT_INIT_MAX_LEN (32768) rather than reserving a bit.
// A bit-15 test agrees with the real rule everywhere except at 32768 itself,
// which has the bit set but is a written extent of maximum length -- so that
// row is the whole point of this table.
//
// Getting it wrong is silent: fs.go's file() skips extents it believes are
// unwritten, so a dropped 32768-block extent leaves a 128 MiB hole in the
// block table and Read() serves zeros for it. No error anywhere.
func TestExtentLenBoundary(t *testing.T) {
	tests := []struct {
		name   string
		raw    uint16
		uninit bool
		length uint16
	}{
		{"empty", 0, false, 0},
		{"one block", 1, false, 1},
		{"written, one under the boundary", 32766, false, 32766},
		{"written, just under the boundary", 32767, false, 32767},
		{"written, maximum length", 32768, false, 32768},
		{"unwritten, one block", 32769, true, 1},
		{"unwritten, mid range", 40000, true, 7232},
		{"unwritten, maximum length", 65535, true, 32767},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := Extent{Len: tt.raw}
			if got := e.IsUninitialized(); got != tt.uninit {
				t.Errorf("Extent{Len: %d}.IsUninitialized() = %v, want %v",
					tt.raw, got, tt.uninit)
			}
			if got := e.GetLen(); got != tt.length {
				t.Errorf("Extent{Len: %d}.GetLen() = %d, want %d",
					tt.raw, got, tt.length)
			}
		})
	}
}

// TestExtentMaxLenReachesBlockTable is the regression this fix exists for.
//
// A file built by the skeleton plugin gets extents capped at
// EXT4_MAX_EXTENT_BLOCKS, which is exactly 32768 -- so every file over 128 MiB
// carries at least one maximum-length extent. Under the old bit-15 test those
// extents reported IsUninitialized() == true and GetLen() == 0, were skipped
// when the block table was built, and read back as zeros.
func TestExtentMaxLenReachesBlockTable(t *testing.T) {
	// A three-extent file: a short run, then a maximum-length run, then the
	// remainder. Only the middle one sits on the boundary.
	extents := []Extent{
		{Block: 0, Len: 15321},
		{Block: 15321, Len: 32768},
		{Block: 48089, Len: 31743},
	}

	var total uint32
	for _, e := range extents {
		if e.IsUninitialized() {
			t.Fatalf("extent at logical block %d (Len %d) was treated as "+
				"unwritten; its blocks would be missing from the block table "+
				"and read back as zeros", e.Block, e.Len)
		}
		total += uint32(e.GetLen())
	}

	const want = 15321 + 32768 + 31743
	if total != want {
		t.Errorf("extents cover %d blocks, want %d", total, want)
	}
}
