package ext4

import (
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/xerrors"
)

var (
	_ fs.File     = &File{}
	_ fs.FileInfo = &FileInfo{}
	_ fs.DirEntry = dirEntry{}
)

// File is implemented io/fs File interface
type File struct {
	FileInfo
	fs        *FileSystem
	filePath  string
	pos       int64 // byte offset of the next Read
	size      int64
	blockSize int64

	// runs maps the file's logical blocks to physical ones, sorted by
	// logical block and non-overlapping. A logical block no run covers is a
	// hole and reads as zeros.
	//
	// This used to be a map with one entry per block, built on every Open
	// and held for the whole read: ~6.5 GiB of heap for a 1 TiB file. One
	// entry per contiguous run costs the same whatever the file size.
	runs []blockRun
	// lastRun is the index of the run the previous Read used. Reads are
	// sequential, so the next block is nearly always in that run or the one
	// after it and the binary search is skipped.
	lastRun int
}

// blockRun is a run of count logical blocks starting at logical, stored
// contiguously on the device starting at block physical.
type blockRun struct {
	logical  int64
	physical int64
	count    int64
}

// FileInfo is implemented io/fs FileInfo interface
type FileInfo struct {
	name  string
	inode *Inode
	ino   int64
}

// Type dirEntry is implemented io/fs DirEntry interface
type dirEntry struct {
	FileInfo
}

func (d dirEntry) Type() fs.FileMode {
	return d.FileInfo.Mode().Type()
}

func (f FileInfo) IsSymlink() bool {
	return f.Mode()&fs.ModeSymlink != 0
}

func (d dirEntry) Info() (fs.FileInfo, error) { return d.FileInfo, nil }

func (f File) Dir() string {
	dir, _ := filepath.Split(f.filePath)
	return dir
}

func (f File) FilePath() string {
	return f.filePath
}

func (fi FileInfo) Name() string {
	return fi.name
}

func (fi FileInfo) Size() int64 {
	return fi.inode.GetSize()
}

func (fi FileInfo) Mode() fs.FileMode {
	m := fi.inode.Mode
	mode := fs.FileMode(m & 0o777)

	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}

	switch m & FileTypeMask {
	case FileTypeSocket:
		mode |= fs.ModeSocket
	case FileTypeSymlink:
		mode |= fs.ModeSymlink
	case FileTypeRegular:
	case FileTypeBlockDevice:
		mode |= fs.ModeDevice
	case FileTypeDir:
		mode |= fs.ModeDir
	case FileTypeCharDevice:
		mode |= fs.ModeCharDevice | fs.ModeDevice
	case FileTypeFifo:
		mode |= fs.ModeNamedPipe
	default:
		mode |= fs.ModeIrregular
	}

	return mode
}

func (fi FileInfo) ModTime() time.Time {
	return time.Unix(int64(fi.inode.Mtime), 0)
}

func (fi FileInfo) IsDir() bool {
	return fi.inode.IsDir()
}

func (fi FileInfo) Inode() *Inode {
	return fi.inode
}

func (fi FileInfo) InodeNumber() int64 {
	return fi.ino
}

func (fi FileInfo) Sys() interface{} {
	if fi.inode == nil {
		return nil
	}
	return fi.inode
}

func (f *File) Stat() (fs.FileInfo, error) {
	return &f.FileInfo, nil
}

// findRun returns the run covering logical block blk. When blk is in a hole
// it returns nil and the logical block at which the hole ends, or -1 if the
// hole runs to the end of the file.
func (f *File) findRun(blk int64) (*blockRun, int64) {
	i := f.lastRun
	if i >= len(f.runs) || f.runs[i].logical > blk ||
		(i+1 < len(f.runs) && f.runs[i+1].logical <= blk) {
		// Last run starting at or before blk; -1 if there is none.
		i = sort.Search(len(f.runs), func(k int) bool {
			return f.runs[k].logical > blk
		}) - 1
	}
	if i >= 0 {
		f.lastRun = i
		if r := &f.runs[i]; blk < r.logical+r.count {
			return r, 0
		}
	}
	if i+1 < len(f.runs) {
		return nil, f.runs[i+1].logical
	}
	return nil, -1
}

// Read fills p with as much as one contiguous piece of the file allows: up to
// the end of the current run of blocks, the end of the current hole, or the
// end of the file. A piece of stored data is fetched with a single ReadAt.
//
// ReadAt does not use the offset of the reader the FileSystem shares between
// its Files, so different Files can be read concurrently. A single File is
// not safe for concurrent use.
func (f *File) Read(p []byte) (int, error) {
	if f.pos >= f.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}

	n := f.size - f.pos
	run, holeEnd := f.findRun(f.pos / f.blockSize)
	switch {
	case run != nil:
		if left := (run.logical+run.count)*f.blockSize - f.pos; left < n {
			n = left
		}
	case holeEnd >= 0:
		if left := holeEnd*f.blockSize - f.pos; left < n {
			n = left
		}
	}
	if int64(len(p)) < n {
		n = int64(len(p))
	}
	p = p[:n]

	if run == nil {
		for i := range p {
			p[i] = 0
		}
		f.pos += n
		return int(n), nil
	}

	offset := run.physical*f.blockSize + (f.pos - run.logical*f.blockSize)
	rn, err := f.fs.r.ReadAt(p, offset)
	if rn != len(p) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return 0, xerrors.Errorf("failed to read block: actual(%d), expected(%d): %w", rn, len(p), err)
	}
	// io.ReaderAt may return io.EOF along with a full read at the end of the input.
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, xerrors.Errorf("failed to read block: %w", err)
	}
	f.pos += n
	return int(n), nil
}

func (f *File) Close() error {
	return nil
}
