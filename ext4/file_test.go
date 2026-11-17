package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"sync"
	"testing"
)

// newTestFile creates a File with the given parameters for testing Read().
// The dataTable maps logical block numbers to byte offsets in the image.
func newTestFile(image []byte, blockSize int64, fileSize int64, table dataTable) *File {
	// One single-block run per mapped block, in logical order.
	var runs []blockRun
	for blk := int64(0); blk*blockSize < fileSize; blk++ {
		if offset, ok := table[blk]; ok {
			runs = append(runs, blockRun{logical: blk, physical: offset / blockSize, count: 1})
		}
	}

	r := io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image)))
	fs := &FileSystem{r: r}
	inode := &Inode{SizeLo: uint32(fileSize)}

	return &File{
		FileInfo: FileInfo{
			name:  "test",
			inode: inode,
		},
		fs:        fs,
		blockSize: blockSize,
		runs:      runs,
		size:      fileSize,
	}
}

// dataTable maps a logical block number to a byte offset in the image.
type dataTable map[int64]int64

func readAll(t *testing.T, f *File) []byte {
	t.Helper()
	var result []byte
	buf := make([]byte, 256)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read error: %v", err)
		}
	}
	return result
}

func TestFileRead_SparseLastBlock(t *testing.T) {
	// File with blockSize=512, size=2000 (4 blocks: 3 full + 1 partial of 464 bytes).
	// Block 0-2 have data, block 3 is sparse (not in table).
	const blockSize = 512
	const fileSize = 2000

	// Build image with 3 blocks of known data
	image := make([]byte, blockSize*3)
	for i := 0; i < 3; i++ {
		for j := 0; j < blockSize; j++ {
			image[i*blockSize+j] = byte(i + 1) // block 0: 0x01, block 1: 0x02, block 2: 0x03
		}
	}

	table := dataTable{
		0: 0,
		1: blockSize,
		2: blockSize * 2,
		// block 3 is sparse (missing from table)
	}

	f := newTestFile(image, blockSize, fileSize, table)
	data := readAll(t, f)

	if int64(len(data)) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(data), fileSize)
	}

	// Verify first 3 blocks have correct data
	for i := 0; i < 3; i++ {
		for j := 0; j < blockSize; j++ {
			if data[i*blockSize+j] != byte(i+1) {
				t.Fatalf("block %d byte %d: got %#x, want %#x", i, j, data[i*blockSize+j], byte(i+1))
			}
		}
	}

	// Verify last partial block (464 bytes) is all zeros
	sparseStart := 3 * blockSize
	remaining := fileSize - sparseStart
	for j := 0; j < remaining; j++ {
		if data[sparseStart+j] != 0 {
			t.Fatalf("sparse block byte %d: got %#x, want 0", j, data[sparseStart+j])
		}
	}
}

func TestFileRead_SparseFullBlock(t *testing.T) {
	// File with blockSize=512, size=1024. Block 0 has data, block 1 is sparse.
	const blockSize = 512
	const fileSize = 1024

	image := make([]byte, blockSize)
	for j := 0; j < blockSize; j++ {
		image[j] = 0xAA
	}

	table := dataTable{
		0: 0,
		// block 1 is sparse
	}

	f := newTestFile(image, blockSize, fileSize, table)
	data := readAll(t, f)

	if int64(len(data)) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(data), fileSize)
	}

	// Block 0: 0xAA
	for j := 0; j < blockSize; j++ {
		if data[j] != 0xAA {
			t.Fatalf("block 0 byte %d: got %#x, want 0xAA", j, data[j])
		}
	}

	// Block 1: all zeros
	for j := blockSize; j < fileSize; j++ {
		if data[j] != 0 {
			t.Fatalf("sparse block byte %d: got %#x, want 0", j-blockSize, data[j])
		}
	}
}

func TestFileRead_AllSparse(t *testing.T) {
	// Entirely sparse file: blockSize=512, size=768 (1 full + 1 partial).
	const blockSize = 512
	const fileSize = 768

	image := make([]byte, 0) // no data blocks
	table := dataTable{}     // everything sparse

	f := newTestFile(image, blockSize, fileSize, table)
	data := readAll(t, f)

	if int64(len(data)) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(data), fileSize)
	}

	for i, b := range data {
		if b != 0 {
			t.Fatalf("byte %d: got %#x, want 0", i, b)
		}
	}
}

func TestFileInfoExposesInode(t *testing.T) {
	inode := &Inode{Mode: FileTypeRegular | 0o644, LinksCount: 3}
	fi := FileInfo{name: "foo", inode: inode, ino: 42}

	if got := fi.Inode(); got != inode {
		t.Fatalf("Inode() = %p, want %p", got, inode)
	}
	if got := fi.InodeNumber(); got != 42 {
		t.Fatalf("InodeNumber() = %d, want 42", got)
	}
	sys, ok := fi.Sys().(*Inode)
	if !ok || sys != inode {
		t.Fatalf("Sys() = %T %v, want *Inode %p", fi.Sys(), fi.Sys(), inode)
	}
}

func TestFileInfoSysNilWhenInodeMissing(t *testing.T) {
	var fi FileInfo
	if fi.Sys() != nil {
		t.Fatalf("Sys() = %v, want nil", fi.Sys())
	}
}

func TestReadDirInfoRootSetsInodeNumber(t *testing.T) {
	const blockSize int64 = 4096
	const inodeTableBlock = 1
	const inodeSize = 256

	image := make([]byte, blockSize*2)
	// inode 2 is the second table slot (1-based).
	offset := int(blockSize)*inodeTableBlock + 1*inodeSize
	binary.LittleEndian.PutUint16(image[offset:offset+2], FileTypeDir|0o755)

	r := io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image)))
	gd := GroupDescriptor{}
	gd.InodeTableLo = inodeTableBlock
	ext4fs := &FileSystem{
		r: r,
		sb: Superblock{
			LogBlockSize: 2, BlockCountLo: testBlockCount,
			InodeSize:     inodeSize,
			InodePerGroup: 16,
		},
		gds:   []GroupDescriptor{gd},
		cache: &mockCache[string, any]{},
	}

	info, err := ext4fs.ReadDirInfo("/")
	if err != nil {
		t.Fatalf("ReadDirInfo(/): %v", err)
	}
	fi, ok := info.(FileInfo)
	if !ok {
		t.Fatalf("ReadDirInfo(/) returned %T, want FileInfo", info)
	}
	if fi.InodeNumber() != rootInodeNumber {
		t.Fatalf("root InodeNumber() = %d, want %d", fi.InodeNumber(), rootInodeNumber)
	}
	if !fi.IsDir() {
		t.Fatal("root FileInfo.IsDir() = false, want true")
	}
}

func TestFileRead_Concurrent(t *testing.T) {
	// Two Files opened from the same FileSystem (sharing the same reader) point
	// to different block ranges. Reading them concurrently must not mix up the
	// data, which happens if Read depends on the reader's shared offset.
	const blockSize = 1024
	const blocksPerFile = 64
	const fileSize = blockSize * blocksPerFile

	// Every block in the image has a distinct byte value, so reading a wrong
	// block is always detected.
	image := make([]byte, blockSize*blocksPerFile*2)
	for i := 0; i < blocksPerFile*2; i++ {
		for j := 0; j < blockSize; j++ {
			image[i*blockSize+j] = byte(i)
		}
	}

	table1 := dataTable{}
	table2 := dataTable{}
	for i := int64(0); i < blocksPerFile; i++ {
		table1[i] = i * blockSize
		table2[i] = (i + blocksPerFile) * blockSize
	}

	f1 := newTestFile(image, blockSize, fileSize, table1)
	f2 := newTestFile(image, blockSize, fileSize, table2)
	f2.fs = f1.fs

	// t.Fatal must not be called from other goroutines, so errors are
	// collected here and checked after all reads finish.
	var wg sync.WaitGroup
	results := make([][]byte, 2)
	errs := make([]error, 2)
	for i, f := range []*File{f1, f2} {
		wg.Add(1)
		go func(i int, f *File) {
			defer wg.Done()
			results[i], errs[i] = io.ReadAll(f)
		}(i, f)
	}
	wg.Wait()

	want := [][]byte{
		image[:fileSize],
		image[fileSize:],
	}
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("file %d: Read error: %v", i+1, errs[i])
		}
		if !bytes.Equal(results[i], want[i]) {
			t.Errorf("file %d: read data does not match the expected block range", i+1)
		}
	}
}

func TestFileInfoMode(t *testing.T) {
	tests := []struct {
		name     string
		imode    uint16
		wantType fs.FileMode
		wantPerm fs.FileMode
	}{
		{"regular 0644", 0x81A4, 0, 0o644},
		{"directory 0755", 0x41ED, fs.ModeDir, 0o755},
		{"symlink 0777", 0xA1FF, fs.ModeSymlink, 0o777},
		{"socket 0755", 0xC1ED, fs.ModeSocket, 0o755},
		{"fifo 0644", 0x11A4, fs.ModeNamedPipe, 0o644},
		{"char device 0666", 0x21B6, fs.ModeCharDevice | fs.ModeDevice, 0o666},
		{"block device 0660", 0x61B0, fs.ModeDevice, 0o660},
		{"setuid", 0x89A4, 0, 0o644 | fs.ModeSetuid},
		{"setgid", 0x85A4, 0, 0o644 | fs.ModeSetgid},
		{"sticky", 0x83A4, 0, 0o644 | fs.ModeSticky},
		{"setuid+setgid+sticky", 0x8FA4, 0, 0o644 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fi := FileInfo{
				name:  "test",
				inode: &Inode{Mode: tt.imode},
			}
			got := fi.Mode()

			if got.Type() != tt.wantType {
				t.Errorf("Type() = %v, want %v", got.Type(), tt.wantType)
			}
			if got.Perm() != tt.wantPerm.Perm() {
				t.Errorf("Perm() = %o, want %o", got.Perm(), tt.wantPerm.Perm())
			}
			if tt.wantPerm&fs.ModeSetuid != 0 && got&fs.ModeSetuid == 0 {
				t.Error("ModeSetuid not set")
			}
			if tt.wantPerm&fs.ModeSetgid != 0 && got&fs.ModeSetgid == 0 {
				t.Error("ModeSetgid not set")
			}
			if tt.wantPerm&fs.ModeSticky != 0 && got&fs.ModeSticky == 0 {
				t.Error("ModeSticky not set")
			}

			// Verify Go standard API works correctly
			switch tt.wantType {
			case fs.ModeDir:
				if !got.IsDir() {
					t.Error("IsDir() = false, want true")
				}
				if got.IsRegular() {
					t.Error("IsRegular() = true, want false")
				}
			case 0:
				if !got.IsRegular() {
					t.Error("IsRegular() = false, want true")
				}
				if got.IsDir() {
					t.Error("IsDir() = true, want false")
				}
			default:
				if got.IsDir() {
					t.Error("IsDir() should be false for non-dir type")
				}
				if got.IsRegular() {
					t.Error("IsRegular() should be false for non-regular type")
				}
			}
		})
	}
}

// TestFileRead_UninitializedExtentReadsZeros verifies that file() skips
// uninitialized extents, and Read() returns zeros for those blocks via the
// sparse path.
func TestFileRead_UninitializedExtentReadsZeros(t *testing.T) {
	const blockSize = 4096
	// 4 blocks total: block 0-1 initialized, block 2-3 uninitialized
	const fileSize = blockSize * 4

	// Build image: 4 blocks of disk data.
	// Blocks at physical position 0-1 contain 0xAA (will be mapped to logical 0-1).
	// Blocks at physical position 2-3 contain 0xBB (uninitialized extent points here,
	// but these should NOT be read).
	image := make([]byte, blockSize*4)
	for i := 0; i < blockSize*2; i++ {
		image[i] = 0xAA
	}
	for i := blockSize * 2; i < blockSize*4; i++ {
		image[i] = 0xBB
	}

	r := io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image)))

	// Build inode with extent tree in BlockOrExtents:
	// ExtentHeader (depth=0, 2 entries) + 2 Extents
	var extData [60]byte
	buf := bytes.NewBuffer(extData[:0])
	binary.Write(buf, binary.LittleEndian, ExtentHeader{
		Magic: 0xF30A, Entries: 2, Max: 4, Depth: 0,
	})
	// Extent 1: blocks 0-1, initialized, physical block 0
	binary.Write(buf, binary.LittleEndian, Extent{
		Block: 0, Len: 2, StartHi: 0, StartLo: 0,
	})
	// Extent 2: blocks 2-3, uninitialized (bit 15 set), physical block 2
	binary.Write(buf, binary.LittleEndian, Extent{
		Block: 2, Len: 0x8002, StartHi: 0, StartLo: 2,
	})
	copy(extData[:], buf.Bytes())

	inode := &Inode{
		SizeLo: fileSize,
		Flags:  EXTENTS_FL,
	}
	copy(inode.BlockOrExtents[:], extData[:])

	fs := &FileSystem{
		r:  r,
		sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount}, // 4096
	}

	fi := FileInfo{name: "test", inode: inode}
	f, err := fs.file(fi, "test")
	if err != nil {
		t.Fatalf("file() error: %v", err)
	}

	data := readAll(t, f)
	if int64(len(data)) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(data), fileSize)
	}

	// Blocks 0-1: should be 0xAA (initialized extent)
	for i := 0; i < blockSize*2; i++ {
		if data[i] != 0xAA {
			t.Fatalf("byte %d (initialized): got %#x, want 0xAA", i, data[i])
		}
	}

	// Blocks 2-3: should be 0x00 (uninitialized extent → zeros, NOT 0xBB)
	for i := blockSize * 2; i < fileSize; i++ {
		if data[i] != 0 {
			t.Fatalf("byte %d (uninitialized): got %#x, want 0x00", i, data[i])
		}
	}
}

// countingReaderAt counts the ReadAt calls that reach the device.
type countingReaderAt struct {
	r     io.ReaderAt
	calls int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	return c.r.ReadAt(p, off)
}

// extentInode builds an inode whose extent tree is a single leaf in the inode
// itself, which holds at most four extents.
func extentInode(t *testing.T, size int64, extents ...Extent) *Inode {
	t.Helper()
	if len(extents) > 4 {
		t.Fatalf("%d extents do not fit in an inode", len(extents))
	}
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, ExtentHeader{
		Magic: 0xF30A, Entries: uint16(len(extents)), Max: 4, Depth: 0,
	})
	for _, e := range extents {
		binary.Write(&buf, binary.LittleEndian, e)
	}
	inode := &Inode{SizeLo: uint32(size), SizeHigh: uint32(size >> 32), Flags: EXTENTS_FL}
	copy(inode.BlockOrExtents[:], buf.Bytes())
	return inode
}

// TestFileRead_ExtentsHolesUnwrittenShortTail reads a file laid out with
// everything the run lookup has to get right -- extents out of physical
// order, a hole between them, an unwritten extent, and a last block shorter
// than a block -- and checks every byte, at buffer sizes that split blocks,
// match them, and span whole extents.
func TestFileRead_ExtentsHolesUnwrittenShortTail(t *testing.T) {
	const blockSize = 4096
	const fileSize = 11*blockSize + 100

	// Every device byte is distinct per block and position, so a read from
	// the wrong place never passes.
	image := make([]byte, 64*blockSize)
	for i := range image {
		image[i] = byte(i/blockSize*7 + i%251 + 1)
	}

	inode := extentInode(t, fileSize,
		Extent{Block: 0, Len: 3, StartLo: 40},      // blocks 0-2
		Extent{Block: 5, Len: 2, StartLo: 10},      // blocks 3-4 are a hole
		Extent{Block: 7, Len: 0x8002, StartLo: 20}, // blocks 7-8 unwritten
		Extent{Block: 9, Len: 3, StartLo: 30},      // blocks 9-11, 11 is the short tail
	)

	want := make([]byte, fileSize)
	copy(want[0:], image[40*blockSize:43*blockSize])
	copy(want[5*blockSize:], image[10*blockSize:12*blockSize])
	copy(want[9*blockSize:], image[30*blockSize:33*blockSize]) // cut at fileSize

	for _, bufSize := range []int{1, 100, blockSize - 1, blockSize, blockSize + 1, 3 * blockSize, 1 << 20} {
		ext4fs := &FileSystem{
			r:  io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image))),
			sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount},
		}
		f, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test")
		if err != nil {
			t.Fatalf("file(): %v", err)
		}

		var got []byte
		buf := make([]byte, bufSize)
		for {
			// Poison the buffer: a hole must be written as zeros, not left
			// holding whatever the previous read put there.
			for i := range buf {
				buf[i] = 0xEE
			}
			n, err := f.Read(buf)
			got = append(got, buf[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("buffer %d: Read: %v", bufSize, err)
			}
			if n == 0 {
				t.Fatalf("buffer %d: Read returned 0 bytes and no error", bufSize)
			}
		}
		if !bytes.Equal(got, want) {
			t.Errorf("buffer %d: read %d bytes that differ from the expected %d", bufSize, len(got), len(want))
		}
	}
}

// TestFileRead_OneReadAtPerRun pins the cost of a read. A caller with a large
// buffer gets a whole extent per Read, fetched with one ReadAt. This used to
// be one Read per block and eight 512-byte ReadAt calls under each.
func TestFileRead_OneReadAtPerRun(t *testing.T) {
	const blockSize = 4096
	const fileSize = 512 * blockSize // 2 MiB

	image := make([]byte, 600*blockSize)
	for i := range image {
		image[i] = byte(i/blockSize + i%13)
	}
	inode := extentInode(t, fileSize,
		Extent{Block: 0, Len: 256, StartLo: 300},
		Extent{Block: 256, Len: 256, StartLo: 8},
	)

	counter := &countingReaderAt{r: bytes.NewReader(image)}
	ext4fs := &FileSystem{
		r:  io.NewSectionReader(counter, 0, int64(len(image))),
		sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount},
	}
	f, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test")
	if err != nil {
		t.Fatalf("file(): %v", err)
	}

	var got []byte
	reads := 0
	buf := make([]byte, 4<<20)
	for {
		n, err := f.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		reads++
	}

	want := append(append([]byte{}, image[300*blockSize:556*blockSize]...), image[8*blockSize:264*blockSize]...)
	if !bytes.Equal(got, want) {
		t.Fatal("read data does not match the extents")
	}
	if reads != 2 || counter.calls != 2 {
		t.Errorf("%d Reads and %d device ReadAt calls for 2 extents, want 2 and 2", reads, counter.calls)
	}
}

// TestFileOpen_MapsExtentsNotBlocks is the memory bound: what a File holds
// grows with the number of extents, not the number of blocks. A fully
// allocated 128 MiB extent is one entry, where the per-block map held 32768.
func TestFileOpen_MapsExtentsNotBlocks(t *testing.T) {
	const blockSize = 4096
	inode := extentInode(t, 3*32768*blockSize,
		Extent{Block: 0, Len: 32768, StartLo: 100000},
		Extent{Block: 32768, Len: 32768, StartLo: 200000},
		Extent{Block: 65536, Len: 32768, StartLo: 300000},
	)
	ext4fs := &FileSystem{sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount}}
	f, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test")
	if err != nil {
		t.Fatalf("file(): %v", err)
	}
	if len(f.runs) != 3 {
		t.Errorf("%d runs for 3 extents, want 3", len(f.runs))
	}
}

// TestFileOpen_RejectsOverlappingExtents: two extents claiming one logical
// block is corruption, with no way to tell which holds the data.
func TestFileOpen_RejectsOverlappingExtents(t *testing.T) {
	inode := extentInode(t, 8*4096,
		Extent{Block: 0, Len: 4, StartLo: 10},
		Extent{Block: 3, Len: 4, StartLo: 20},
	)
	ext4fs := &FileSystem{sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount}}
	if _, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test"); err == nil {
		t.Fatal("file() accepted overlapping extents")
	}
}

// TestFileFromBlock_JoinsContiguousBlocks: a block-mapped file gets one run
// per contiguous stretch, and reads back across the joins and the hole.
func TestFileFromBlock_JoinsContiguousBlocks(t *testing.T) {
	const blockSize = 4096
	// Blocks 20,21,22 contiguous; a hole; then 9 and 10.
	inode := buildBlockAddressingInode([12]uint32{20, 21, 22, 0, 9, 10}, 0, 0, 0, 6*blockSize)

	image := make([]byte, 32*blockSize)
	for i := range image {
		image[i] = byte(i/blockSize*3 + i%17 + 1)
	}
	ext4fs := &FileSystem{
		r:     io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image))),
		sb:    Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount},
		cache: &mockCache[string, any]{},
	}
	f, err := ext4fs.fileFromBlock(FileInfo{name: "test", inode: inode}, "test")
	if err != nil {
		t.Fatalf("fileFromBlock(): %v", err)
	}
	if len(f.runs) != 2 {
		t.Errorf("%d runs, want 2: %+v", len(f.runs), f.runs)
	}

	want := make([]byte, 6*blockSize)
	copy(want, image[20*blockSize:23*blockSize])
	copy(want[4*blockSize:], image[9*blockSize:11*blockSize])
	if got := readAll(t, f); !bytes.Equal(got, want) {
		t.Error("read data does not match the block map")
	}
}

// TestFileRead_TruncatedDeviceIsNotEOF: the extent is valid by the superblock
// but the device ends partway through it. That has to be an error no caller
// can take for the end of the file -- not by == and not by errors.Is -- or a
// cut-short image restores as a shorter file with no error.
func TestFileRead_TruncatedDeviceIsNotEOF(t *testing.T) {
	const blockSize = 4096
	const fileSize = 4 * blockSize

	// The extent starts at block 1 and the device stops 100 bytes into the
	// file's third block.
	image := make([]byte, 3*blockSize+100)
	for i := range image {
		image[i] = byte(i%251 + 1)
	}
	inode := extentInode(t, fileSize, Extent{Block: 0, Len: 4, StartLo: 1})

	for _, bufSize := range []int{blockSize, 1 << 20} {
		ext4fs := &FileSystem{
			r:  io.NewSectionReader(bytes.NewReader(image), 0, int64(len(image))),
			sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount},
		}
		f, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test")
		if err != nil {
			t.Fatalf("file(): %v", err)
		}

		buf := make([]byte, bufSize)
		var total int
		for err == nil {
			var n int
			n, err = f.Read(buf)
			total += n
		}
		if total >= fileSize {
			t.Fatalf("buffer %d: read %d bytes from a device that holds fewer", bufSize, total)
		}
		if errors.Is(err, io.EOF) {
			t.Errorf("buffer %d: truncated device reported as end of file: %v", bufSize, err)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("buffer %d: error is not io.ErrUnexpectedEOF: %v", bufSize, err)
		}

		// A failed Read does not move the position: asking again fails the
		// same way rather than skipping the missing bytes or ending the file.
		if n, err := f.Read(buf); n != 0 || err == nil || errors.Is(err, io.EOF) {
			t.Errorf("buffer %d: Read after the error returned %d, %v", bufSize, n, err)
		}
	}
}

// eofWithDataReaderAt returns io.EOF together with a read that reaches the
// end of the input, which io.ReaderAt allows.
type eofWithDataReaderAt struct{ data []byte }

func (r eofWithDataReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := copy(p, r.data[off:])
	if off+int64(n) == int64(len(r.data)) {
		return n, io.EOF
	}
	return n, nil
}

// TestFileRead_FullReadWithEOFIsNotAnError: a complete read that arrives with
// io.EOF, because the file ends where the device does, is a successful read.
func TestFileRead_FullReadWithEOFIsNotAnError(t *testing.T) {
	const blockSize = 4096
	image := make([]byte, 3*blockSize)
	for i := range image {
		image[i] = byte(i%251 + 1)
	}
	inode := extentInode(t, 2*blockSize, Extent{Block: 0, Len: 2, StartLo: 1})
	ext4fs := &FileSystem{
		r:  io.NewSectionReader(eofWithDataReaderAt{image}, 0, int64(len(image))),
		sb: Superblock{LogBlockSize: 2, BlockCountLo: testBlockCount},
	}
	f, err := ext4fs.file(FileInfo{name: "test", inode: inode}, "test")
	if err != nil {
		t.Fatalf("file(): %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, image[blockSize:]) {
		t.Error("read data does not match the device")
	}
}
