//go:build e2e && linux

package e2e

// TestClone reads an ext4 image with this library, writes every entry out to
// a directory, and compares the result with the same image mounted by the
// kernel. The kernel is the reference: anything the library reads wrongly
// shows up as a difference.
//
// Environment (set by scripts/e2e-clone.sh):
//
//	EXT4_E2E_IMAGE   the ext4 image file
//	EXT4_E2E_SOURCE  where that image is mounted read-only
//	EXT4_E2E_CLONE   where to write the clone (must not exist, or be empty)
//
// Must run as root: the clone recreates owners, device nodes and trusted.*
// and security.* attributes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/trilioData/go-ext4-filesystem/ext4"
)

func env(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; run scripts/e2e-clone.sh", name)
	}
	return v
}

func TestClone(t *testing.T) {
	image := env(t, "EXT4_E2E_IMAGE")
	source := env(t, "EXT4_E2E_SOURCE")
	clone := env(t, "EXT4_E2E_CLONE")
	if os.Geteuid() != 0 {
		t.Fatal("must run as root")
	}

	f, err := os.Open(image)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := ext4.NewFS(*io.NewSectionReader(f, 0, st.Size()), nil)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}

	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	c := &cloner{fs: fsys, dest: clone, links: map[int64]string{}}
	if err := c.cloneTree(); err != nil {
		t.Fatalf("clone: %v", err)
	}
	t.Logf("cloned: %+v", c.stats)

	compareTrees(t, source, clone)
}

// ---------------------------------------------------------------- cloner

type cloneStats struct {
	Dirs, Files, Symlinks, Hardlinks, Special, Xattrs int
	Bytes                                             int64
}

// cloner is a minimal restore: one walk, attributes applied per entry,
// directories last and deepest first.
type cloner struct {
	fs    *ext4.FileSystem
	dest  string
	links map[int64]string // image inode -> first clone path, for hardlinks
	dirs  []dirAttr
	stats cloneStats
}

type dirAttr struct {
	local string
	info  ext4.FileInfo
}

func (c *cloner) cloneTree() error {
	root, err := c.fs.Lstat("/")
	if err != nil {
		return err
	}
	if err := c.walk("", root.(ext4.FileInfo)); err != nil {
		return err
	}
	for i := len(c.dirs) - 1; i >= 0; i-- { // deepest first
		if err := c.applyAttrs(c.dirs[i].local, c.dirs[i].info, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *cloner) walk(rel string, dirInfo ext4.FileInfo) error {
	local := filepath.Join(c.dest, rel)
	c.dirs = append(c.dirs, dirAttr{local, dirInfo})
	names, err := c.fs.ReadDirNames(rel)
	if err != nil {
		return fmt.Errorf("ReadDirNames(%q): %w", rel, err)
	}
	sort.Strings(names)
	for _, name := range names {
		childRel := path.Join(rel, name)
		fi, err := c.fs.Lstat(childRel)
		if err != nil {
			return fmt.Errorf("Lstat(%q): %w", childRel, err)
		}
		info := fi.(ext4.FileInfo)
		if err := c.entry(childRel, info); err != nil {
			return fmt.Errorf("%s: %w", childRel, err)
		}
	}
	return nil
}

func (c *cloner) entry(rel string, info ext4.FileInfo) error {
	local := filepath.Join(c.dest, rel)
	ino := info.Inode()
	mode := info.Mode()

	if mode.IsDir() {
		if err := os.Mkdir(local, 0o700); err != nil {
			return err
		}
		c.stats.Dirs++
		return c.walk(rel, info)
	}

	// Second and later names of an inode become hard links.
	if !mode.IsDir() && ino.LinksCount > 1 {
		if first, ok := c.links[info.InodeNumber()]; ok {
			c.stats.Hardlinks++
			return os.Link(first, local)
		}
		c.links[info.InodeNumber()] = local
	}

	switch {
	case mode&fs.ModeSymlink != 0:
		target, err := c.fs.ReadLink(rel)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, local); err != nil {
			return err
		}
		c.stats.Symlinks++
		return c.applyAttrs(local, info, true)

	case mode.IsRegular():
		if err := c.copyFile(rel, local, info.Size()); err != nil {
			return err
		}
		c.stats.Files++

	default: // fifo, socket, character or block device
		var typ uint32
		switch {
		case mode&fs.ModeNamedPipe != 0:
			typ = syscall.S_IFIFO
		case mode&fs.ModeSocket != 0:
			typ = syscall.S_IFSOCK
		case mode&fs.ModeCharDevice != 0:
			typ = syscall.S_IFCHR
		case mode&fs.ModeDevice != 0:
			typ = syscall.S_IFBLK
		default:
			return fmt.Errorf("unsupported mode %v", mode)
		}
		if err := syscall.Mknod(local, typ|0o600, int(deviceNumber(ino))); err != nil {
			return err
		}
		c.stats.Special++
	}
	return c.applyAttrs(local, info, false)
}

// copyFile copies through the library's reader, seeking over all-zero
// blocks so a sparse source stays sparse.
func (c *cloner) copyFile(rel, local string, size int64) error {
	src, err := c.fs.Open(rel)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()

	const blk = 4096
	buf := make([]byte, 1<<20)
	zero := make([]byte, blk)
	var off int64
	for {
		n, rerr := io.ReadFull(src, buf)
		for i := 0; i < n; i += blk {
			end := i + blk
			if end > n {
				end = n
			}
			if bytes.Equal(buf[i:end], zero[:end-i]) {
				continue
			}
			if _, err := dst.WriteAt(buf[i:end], off+int64(i)); err != nil {
				return err
			}
		}
		off += int64(n)
		c.stats.Bytes += int64(n)
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if off != size {
		return fmt.Errorf("read %d bytes, size is %d", off, size)
	}
	return dst.Truncate(size)
}

// applyAttrs sets xattrs, owner, mode and times, in the order a restore
// must use: xattrs before chmod (a 0444 file refuses user.* otherwise), chown
// before chmod (chown clears setuid/setgid), times last.
func (c *cloner) applyAttrs(local string, info ext4.FileInfo, symlink bool) error {
	ino := info.Inode()
	if !symlink {
		xattrs, err := c.fs.ListXattrs(info.InodeNumber())
		if err != nil {
			return err
		}
		for name, value := range xattrs {
			if err := syscall.Setxattr(local, name, value, 0); err != nil {
				return fmt.Errorf("setxattr %s: %w", name, err)
			}
			c.stats.Xattrs++
		}
	}
	uid := int(uint32(ino.UIDHigh)<<16 | uint32(ino.UID))
	gid := int(uint32(ino.GIDHigh)<<16 | uint32(ino.GID))
	if err := os.Lchown(local, uid, gid); err != nil {
		return err
	}
	if !symlink {
		if err := syscall.Chmod(local, uint32(ino.Mode&0o7777)); err != nil {
			return err
		}
	}
	ts := []syscall.Timespec{inodeTime(ino, ino.Atime, ino.AtimeExtra, 16),
		inodeTime(ino, ino.Mtime, ino.MtimeExtra, 12)}
	return utimesNoFollow(local, ts)
}

// inodeTime assembles a timestamp from its seconds field and, when the inode's
// extra area reaches it, the extra field: two epoch bits extending the
// seconds past 2038, and 30 bits of nanoseconds. needIsize is how much of the
// extra area the field requires (12 for mtime, 16 for atime).
func inodeTime(ino *ext4.Inode, sec, extra uint32, needIsize uint16) syscall.Timespec {
	s := int64(int32(sec))
	var ns int64
	if ino.ExtraIsize >= needIsize {
		s += int64(extra&3) << 32
		ns = int64(extra >> 2)
	}
	return syscall.Timespec{Sec: s, Nsec: ns}
}

// deviceNumber decodes a device inode's major and minor numbers from i_block,
// as ext4_read_inode does: the old 16-bit encoding in word 0, otherwise the
// new 32-bit encoding in word 1. Returned in the kernel's dev_t layout for
// mknod.
func deviceNumber(ino *ext4.Inode) uint64 {
	var major, minor uint32
	if old := binary.LittleEndian.Uint32(ino.BlockOrExtents[0:]); old != 0 {
		major, minor = (old>>8)&0xff, old&0xff
	} else {
		nw := binary.LittleEndian.Uint32(ino.BlockOrExtents[4:])
		major = (nw & 0xfff00) >> 8
		minor = (nw & 0xff) | ((nw >> 12) & 0xfff00)
	}
	return uint64(minor&0xff) | uint64(major&0xfff)<<8 |
		uint64(minor&^0xff)<<12 | uint64(major&^0xfff)<<32
}

func utimesNoFollow(p string, ts []syscall.Timespec) error {
	const atSymlinkNofollow = 0x100
	atFdcwd := -100
	pp, err := syscall.BytePtrFromString(p)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(atFdcwd),
		uintptr(unsafe.Pointer(pp)), uintptr(unsafe.Pointer(&ts[0])), atSymlinkNofollow, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// ---------------------------------------------------------------- comparison

type entryState struct {
	mode     uint32
	uid, gid uint32
	size     int64
	rdev     uint64
	mtime    syscall.Timespec
	nlink    uint64
	ino      uint64
	blocks   int64
	target   string
	hash     [32]byte
	xattrs   map[string]string
}

func stateOf(t *testing.T, p string) entryState {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	e := entryState{mode: st.Mode, uid: st.Uid, gid: st.Gid, size: st.Size, rdev: st.Rdev,
		mtime: st.Mtim, nlink: uint64(st.Nlink), ino: st.Ino, blocks: st.Blocks}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFLNK:
		e.target, _ = os.Readlink(p)
	case syscall.S_IFREG:
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			t.Fatal(err)
		}
		f.Close()
		copy(e.hash[:], h.Sum(nil))
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFLNK {
		e.xattrs = xattrsOf(t, p)
	}
	return e
}

func xattrsOf(t *testing.T, p string) map[string]string {
	t.Helper()
	sz, err := syscall.Listxattr(p, nil)
	if err != nil || sz == 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, _ = syscall.Listxattr(p, buf)
	out := map[string]string{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		vsz, err := syscall.Getxattr(p, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		v := make([]byte, vsz)
		_, _ = syscall.Getxattr(p, name, v)
		out[name] = fmt.Sprintf("%x", v)
	}
	return out
}

func compareTrees(t *testing.T, source, clone string) {
	// Hardlink groups: source inode -> clone inode must be a bijection.
	srcToClone := map[uint64]uint64{}
	cloneToSrc := map[uint64]uint64{}
	var entries, problems int

	err := filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(source, p)
		entries++
		cp := filepath.Join(clone, rel)
		if _, err := os.Lstat(cp); err != nil {
			t.Errorf("%s: missing in clone", rel)
			problems++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		s, c := stateOf(t, p), stateOf(t, cp)
		report := func(what string, a, b interface{}) {
			t.Errorf("%s: %s = %v in clone, %v in source", rel, what, b, a)
			problems++
		}
		if s.mode != c.mode {
			report("mode", fmt.Sprintf("%#o", s.mode), fmt.Sprintf("%#o", c.mode))
		}
		if s.uid != c.uid || s.gid != c.gid {
			report("owner", fmt.Sprintf("%d:%d", s.uid, s.gid), fmt.Sprintf("%d:%d", c.uid, c.gid))
		}
		if s.mode&syscall.S_IFMT != syscall.S_IFDIR && s.size != c.size {
			report("size", s.size, c.size)
		}
		if s.rdev != c.rdev {
			report("rdev", s.rdev, c.rdev)
		}
		if s.mtime != c.mtime {
			report("mtime", s.mtime, c.mtime)
		}
		if s.target != c.target {
			report("symlink target", s.target, c.target)
		}
		if s.hash != c.hash {
			report("content", "source", "differs")
		}
		if fmt.Sprint(s.xattrs) != fmt.Sprint(c.xattrs) {
			report("xattrs", s.xattrs, c.xattrs)
		}
		// A file that is sparse in the source (less than half allocated) must
		// stay sparse. Exact block counts differ legitimately: the clone may
		// sit on a filesystem with another block size, and st_blocks also
		// counts an external xattr block and, with ea_inode, the blocks of
		// the file's EA inodes. Only sparse files of 1 MiB and more are
		// checked; the populate script's files with large xattr values are
		// a few bytes long and never reach it.
		if s.mode&syscall.S_IFMT == syscall.S_IFREG && s.size >= 1<<20 &&
			s.blocks*512 < s.size/2 && c.blocks*512 >= s.size/2 {
			report("allocated bytes (file is no longer sparse)", s.blocks*512, c.blocks*512)
		}
		if s.mode&syscall.S_IFMT != syscall.S_IFDIR {
			if s.nlink != c.nlink {
				report("link count", s.nlink, c.nlink)
			}
			if prev, ok := srcToClone[s.ino]; ok && prev != c.ino {
				report("hardlink group", "shared inode", "separate inode")
			}
			if prev, ok := cloneToSrc[c.ino]; ok && prev != s.ino {
				report("hardlink group", "separate inode", "shared inode")
			}
			srcToClone[s.ino], cloneToSrc[c.ino] = c.ino, s.ino
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing extra in the clone.
	_ = filepath.WalkDir(clone, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(clone, p)
		if _, err := os.Lstat(filepath.Join(source, rel)); errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: extra in clone", rel)
			problems++
		}
		return nil
	})
	t.Logf("compared %d entries, %d problems", entries, problems)
}
