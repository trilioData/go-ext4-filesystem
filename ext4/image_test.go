//go:build linux

package ext4

// End-to-end tests against real ext4 images.
//
// Each test builds a source tree in a temporary directory, turns it into an
// image with `mke2fs -d` (no root needed), adjusts what an unprivileged user
// cannot create directly with `debugfs`, and then reads the image back through
// this package and compares it with the source: contents, types, modes,
// owners, times, xattrs, ACLs, symlinks and hardlinks.
//
// The e2fsprogs tools are required. Without them the tests skip, unless
// EXT4_REQUIRE_TOOLS is set (CI sets it), in which case they fail.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------- helpers

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("EXT4_REQUIRE_TOOLS") != "" {
				t.Fatalf("%s not found and EXT4_REQUIRE_TOOLS is set", tool)
			}
			t.Skipf("%s not found", tool)
		}
	}
}

// imageConfig is one filesystem geometry the reader must handle.
type imageConfig struct {
	name      string
	blockSize int
	inodeSize int
	features  string
	sizeMiB   int
}

const baseFeatures = "none,ext_attr,resize_inode,dir_index,filetype,sparse_super,large_file"

var imageConfigs = []imageConfig{
	// The skeleton plugin's exact geometry.
	{"skeleton-4k-i512", 4096, 512,
		baseFeatures + ",extent,64bit,flex_bg,huge_file,dir_nlink,extra_isize,metadata_csum", 96},
	// A stock ext4 filesystem: journal and 256-byte inodes.
	{"ext4-4k-i256-journal", 4096, 256,
		baseFeatures + ",has_journal,extent,64bit,flex_bg,huge_file,dir_nlink,extra_isize,metadata_csum", 96},
	// 1 KiB blocks: first data block is 1, xattr blocks are small.
	{"ext4-1k-i256", 1024, 256,
		baseFeatures + ",extent,64bit,flex_bg,huge_file,dir_nlink,extra_isize,metadata_csum", 96},
	// 128-byte inodes: no room for inline xattrs or nanosecond times.
	{"ext4-4k-i128", 4096, 128,
		baseFeatures + ",extent,64bit,flex_bg,huge_file,dir_nlink", 96},
	// mkfs.ext4 defaults from e2fsprogs 1.47.1: the checksum seed is stored
	// in the superblock.
	{"ext4-4k-i256-csum-seed", 4096, 256,
		baseFeatures + ",has_journal,extent,64bit,flex_bg,huge_file,dir_nlink,extra_isize,metadata_csum,metadata_csum_seed", 96},
	// ext3-style block maps (no extents): direct and indirect blocks.
	{"ext3-1k-blockmap", 1024, 256, baseFeatures + ",has_journal", 96},
}

func run(t *testing.T, stdin string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// buildImage formats an image from srcDir. Directories are then indexed with
// e2fsck -D so large directories become hash trees, as they would be on a
// filesystem that has been in use.
func buildImage(t *testing.T, cfg imageConfig, srcDir string, debugfsCmds []string) string {
	t.Helper()
	requireTools(t, "mke2fs", "e2fsck", "debugfs")
	img := filepath.Join(t.TempDir(), "fs.img")
	run(t, "", "mke2fs", "-q", "-F", "-t", "ext4",
		"-b", fmt.Sprint(cfg.blockSize), "-I", fmt.Sprint(cfg.inodeSize),
		"-N", "16384", "-O", cfg.features, "-d", srcDir,
		img, fmt.Sprintf("%dM", cfg.sizeMiB))
	if len(debugfsCmds) > 0 {
		out := run(t, strings.Join(debugfsCmds, "\n")+"\n", "debugfs", "-w", "-f", "-", img)
		for _, line := range strings.Split(out, "\n") {
			// debugfs echoes each command as "debugfs: <cmd>" and reports
			// failures on their own lines.
			if line == "" || strings.HasPrefix(line, "debugfs ") || strings.HasPrefix(line, "debugfs:") ||
				strings.HasPrefix(line, "Allocated inode:") {
				continue
			}
			t.Fatalf("debugfs reported: %q\nfull output:\n%s", line, out)
		}
	}
	// -D optimises directories (builds hash-tree indexes); exit status 1
	// means "errors corrected", which -D always reports.
	cmd := exec.Command("e2fsck", "-fyD", img)
	if out, err := cmd.CombinedOutput(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() > 1 {
			t.Fatalf("e2fsck -fyD: %v\n%s", err, out)
		}
	}
	// And the result must be clean.
	run(t, "", "e2fsck", "-fn", img)
	return img
}

func openImage(t *testing.T, img string, offset int64) *FileSystem {
	t.Helper()
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := NewFS(*io.NewSectionReader(f, offset, st.Size()-offset), nil)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fsys
}

func writeFile(t *testing.T, p string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask-proof
		t.Fatal(err)
	}
}

func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

// writeIslands writes 4 KiB of data every stride bytes, leaving holes
// between: one extent per island.
func writeIslands(t *testing.T, rng *rand.Rand, p string, islands int, stride int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < islands; i++ {
		if _, err := f.WriteAt(randBytes(rng, 4096), int64(i)*stride); err != nil {
			t.Fatal(err)
		}
	}
}

// posixACL builds a value in posix_acl_xattr format: version 2, then
// (tag, perm, id) for each entry.
func posixACL(entries ...[3]uint32) []byte {
	b := make([]byte, 4, 4+8*len(entries))
	binary.LittleEndian.PutUint32(b, posixACLXattrVersion)
	for _, e := range entries {
		var x [8]byte
		binary.LittleEndian.PutUint16(x[0:], uint16(e[0]))
		binary.LittleEndian.PutUint16(x[2:], uint16(e[1]))
		binary.LittleEndian.PutUint32(x[4:], e[2])
		b = append(b, x[:]...)
	}
	return b
}

const undef = aclUndefinedID

// sourceXattrs reads every xattr of a path the way the kernel reports them.
func sourceXattrs(t *testing.T, p string) map[string][]byte {
	t.Helper()
	sz, err := syscall.Listxattr(p, nil)
	if err != nil || sz == 0 {
		return nil
	}
	buf := make([]byte, sz)
	sz, err = syscall.Listxattr(p, buf)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, name := range strings.Split(strings.TrimRight(string(buf[:sz]), "\x00"), "\x00") {
		vsz, err := syscall.Getxattr(p, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		v := make([]byte, vsz)
		if _, err := syscall.Getxattr(p, name, v); err != nil {
			t.Fatal(err)
		}
		out[name] = v
	}
	return out
}

func lookupInfo(t *testing.T, fsys *FileSystem, p string) FileInfo {
	t.Helper()
	fi, err := fsys.ReadDirInfo(p)
	if err != nil {
		t.Fatalf("ReadDirInfo(%q): %v", p, err)
	}
	return fi.(FileInfo)
}

func hashReader(t *testing.T, r io.Reader) [32]byte {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return hashReader(t, f)
}

// ---------------------------------------------------------------- source tree

type tree struct {
	dir      string
	aclsOK   bool
	debugfs  []string // commands applied to the image after mke2fs
	bigFiles bool
}

// buildSourceTree creates every shape the reader must handle that an
// unprivileged user can create. The rest goes in t.debugfs.
func buildSourceTree(t *testing.T, cfg imageConfig, big bool) *tree {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	src := t.TempDir()
	tr := &tree{dir: src, bigFiles: big}

	// Regular files around block-size boundaries.
	for _, n := range []int{0, 1, 59, 60, 61, 1023, 1024, 1025, 4095, 4096, 4097, 65536, 1 << 20} {
		writeFile(t, filepath.Join(src, "sizes", fmt.Sprintf("f%07d", n)), randBytes(rng, n), 0o644)
	}
	// Sparse: data, a 10 MiB hole, data, then a hole to the end.
	sp := filepath.Join(src, "sparse", "holes.bin")
	writeIslands(t, rng, sp, 2, 10<<20)
	if err := os.Truncate(sp, 30<<20); err != nil {
		t.Fatal(err)
	}
	// Fragmented: 40 islands means 40 extents, more than the 4 that fit in
	// the inode, so the extent tree needs an index level.
	writeIslands(t, rng, filepath.Join(src, "sparse", "fragmented.bin"), 40, 64<<10)

	// Directories: deep, empty, and large (hash-tree indexed after e2fsck -D).
	deep := src
	for i := 0; i < 25; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", i))
	}
	writeFile(t, filepath.Join(deep, "leaf.txt"), []byte("deep leaf\n"), 0o644)
	if err := os.MkdirAll(filepath.Join(src, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		writeFile(t, filepath.Join(src, "bigdir", fmt.Sprintf("entry-%05d-%s", i, strings.Repeat("x", i%40))),
			[]byte{byte(i)}, 0o644)
	}

	// Names.
	writeFile(t, filepath.Join(src, "names", strings.Repeat("n", 255)), []byte("long name"), 0o644)
	writeFile(t, filepath.Join(src, "names", "ünïcødé 文件 name.txt"), []byte("unicode"), 0o644)
	writeFile(t, filepath.Join(src, "names", ".hidden"), []byte("hidden"), 0o644)

	// Symlinks: fast (target in the inode, < 60 bytes) and slow (in a block),
	// relative, absolute, to a directory, broken, and a loop.
	writeFile(t, filepath.Join(src, "links", "target.txt"), []byte("link target\n"), 0o644)
	writeFile(t, filepath.Join(src, "links", "sub", "inner.txt"), []byte("inner\n"), 0o644)
	symlinks := map[string]string{
		"links/fast-rel":       "target.txt",
		"links/sub/up-rel":     "../target.txt",
		"links/sub/same-dir":   "inner.txt",
		"links/to-dir":         "sub",
		"links/abs":            "/links/target.txt",
		"links/broken":         "does-not-exist",
		"links/slow-rel":       strings.Repeat("./", 40) + "target.txt",
		"links/slow-abs":       "/" + strings.Repeat("very-long-component/", 8) + "x",
		"links/loop-a":         "loop-b",
		"links/loop-b":         "loop-a",
		"links/chain-1":        "chain-2",
		"links/chain-2":        "fast-rel",
		"links/sub/to-sibling": "../sub/inner.txt",
	}
	for name, target := range symlinks {
		if err := os.Symlink(target, filepath.Join(src, name)); err != nil {
			t.Fatal(err)
		}
	}

	// Hardlinks across directories.
	writeFile(t, filepath.Join(src, "hard", "a"), randBytes(rng, 5000), 0o640)
	if err := os.MkdirAll(filepath.Join(src, "hard", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"hard/b", "hard/other/c"} {
		if err := os.Link(filepath.Join(src, "hard", "a"), filepath.Join(src, n)); err != nil {
			t.Fatal(err)
		}
	}

	// Modes, including setuid, setgid and sticky.
	writeFile(t, filepath.Join(src, "modes", "ro"), []byte("r"), 0o644) // 0444 after its xattr
	writeFile(t, filepath.Join(src, "modes", "setuid"), []byte("u"), 0o4755)
	writeFile(t, filepath.Join(src, "modes", "setgid"), []byte("g"), 0o2755)
	if err := os.MkdirAll(filepath.Join(src, "modes", "sticky-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "modes", "sticky-dir"), 0o1777); err != nil {
		t.Fatal(err)
	}

	// FIFO.
	if err := os.MkdirAll(filepath.Join(src, "special"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(src, "special", "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	// User xattrs: small (inline when the inode has room), large (forces an
	// external xattr block), and many.
	writeFile(t, filepath.Join(src, "xattr", "small"), []byte("s"), 0o644)
	writeFile(t, filepath.Join(src, "xattr", "large"), []byte("l"), 0o644)
	writeFile(t, filepath.Join(src, "xattr", "many"), []byte("m"), 0o644)
	writeFile(t, filepath.Join(src, "xattr", "empty-value"), []byte("e"), 0o644)
	setx := func(p, name string, v []byte) {
		if err := syscall.Setxattr(filepath.Join(src, p), name, v, 0); err != nil {
			t.Fatalf("setxattr %s %s: %v", p, name, err)
		}
	}
	setx("xattr/small", "user.a", []byte("1"))
	setx("xattr/large", "user.blob", randBytes(rng, cfg.blockSize/2))
	for i := 0; i < 12; i++ {
		setx("xattr/many", fmt.Sprintf("user.attr-%02d", i), []byte(strings.Repeat("v", i*3)))
	}
	setx("xattr/empty-value", "user.empty", []byte{})
	// An xattr on a read-only file (restore has to set it before chmod).
	setx("modes/ro", "user.tag", []byte("readonly"))
	if err := os.Chmod(filepath.Join(src, "modes", "ro"), 0o444); err != nil {
		t.Fatal(err)
	}

	// POSIX ACLs: an access ACL with named user and group, and a default ACL
	// on a directory. Set as raw xattrs so the expected bytes are exact.
	writeFile(t, filepath.Join(src, "acl", "file"), []byte("acl"), 0o664)
	if err := os.MkdirAll(filepath.Join(src, "acl", "dir"), 0o775); err != nil {
		t.Fatal(err)
	}
	access := posixACL(
		[3]uint32{aclUserObj, 6, undef}, [3]uint32{aclUser, 6, 1234},
		[3]uint32{aclGroupObj, 4, undef}, [3]uint32{aclGroup, 5, 4321},
		[3]uint32{aclMask, 6, undef}, [3]uint32{aclOther, 4, undef})
	def := posixACL(
		[3]uint32{aclUserObj, 7, undef}, [3]uint32{aclUser, 7, 1234},
		[3]uint32{aclGroupObj, 5, undef}, [3]uint32{aclMask, 7, undef},
		[3]uint32{aclOther, 5, undef})
	err1 := syscall.Setxattr(filepath.Join(src, "acl", "file"), XattrPosixACLAccess, access, 0)
	err2 := syscall.Setxattr(filepath.Join(src, "acl", "dir"), XattrPosixACLDefault, def, 0)
	tr.aclsOK = err1 == nil && err2 == nil
	if !tr.aclsOK {
		if os.Getenv("EXT4_REQUIRE_TOOLS") != "" {
			t.Fatalf("source filesystem does not take ACLs (%v, %v) and EXT4_REQUIRE_TOOLS is set", err1, err2)
		}
		t.Logf("source filesystem does not take ACLs (%v, %v); ACL checks skipped", err1, err2)
	}

	// Timestamps: a fixed old mtime and one past 2038.
	writeFile(t, filepath.Join(src, "times", "y2000"), []byte("t"), 0o644)
	if err := os.Chtimes(filepath.Join(src, "times", "y2000"),
		time.Unix(946684800, 123456789), time.Unix(946684800, 987654321)); err != nil {
		t.Fatal(err)
	}

	// Large files, only in long mode.
	if big {
		if cfg.name == "ext3-1k-blockmap" {
			// > 64 MiB of 1 KiB blocks reaches the triple-indirect block.
			writeFile(t, filepath.Join(src, "large", "triple-indirect.bin"), randBytes(rng, 66<<20), 0o644)
		} else {
			// 129 MiB contiguous: crosses the 32768-block maximum extent length.
			writeFile(t, filepath.Join(src, "large", "max-extent.bin"), randBytes(rng, 129<<20), 0o644)
		}
	}

	// What an unprivileged user cannot create on the source.
	tr.debugfs = []string{
		"mkdir /owners",
		"write /dev/null /owners/uid-100000",
		"sif /owners/uid-100000 uid 100000", // sets uid_lo and uid_hi
		"sif /owners/uid-100000 gid 100000",
		"write /dev/null /owners/root",
		"sif /owners/root uid 0",
		"sif /owners/root gid 0",
		"cd /special",
		"mknod chr c 1 3",
		"mknod blk b 7 0",
		"cd /",
		"ea_set /xattr/small security.test sec-value",
		"ea_set /xattr/small trusted.test trusted-value",
	}
	return tr
}

// ---------------------------------------------------------------- the comparison

// TestImageMatchesSource is the main end-to-end test: every entry in the
// source must read back identically, for every geometry.
func TestImageMatchesSource(t *testing.T) {
	for _, cfg := range imageConfigs {
		cfg := cfg
		t.Run(cfg.name, func(t *testing.T) {
			big := !testing.Short() && (cfg.name == "skeleton-4k-i512" || cfg.name == "ext3-1k-blockmap")
			if big {
				cfg.sizeMiB += 160
			}
			tr := buildSourceTree(t, cfg, big)
			img := buildImage(t, cfg, tr.dir, tr.debugfs)
			fsys := openImage(t, img, 0)
			compareTree(t, fsys, tr, cfg)
			checkDebugfsEntries(t, fsys, cfg)
		})
	}
}

func compareTree(t *testing.T, fsys *FileSystem, tr *tree, cfg imageConfig) {
	srcInodes := map[uint64]int64{} // source inode -> image inode
	err := filepath.WalkDir(tr.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(tr.dir, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		t.Run(rel, func(t *testing.T) { compareEntry(t, fsys, tr, rel, p, srcInodes) })
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func compareEntry(t *testing.T, fsys *FileSystem, tr *tree, rel, srcPath string, srcInodes map[uint64]int64) {
	var st syscall.Stat_t
	if err := syscall.Lstat(srcPath, &st); err != nil {
		t.Fatal(err)
	}
	fi := lookupInfo(t, fsys, rel)
	ino := fi.Inode()

	// Type and permission bits, including setuid/setgid/sticky.
	srcInfo, _ := os.Lstat(srcPath)
	if got, want := fi.Mode(), srcInfo.Mode(); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	if got, want := ino.Mode&0o7777, uint16(st.Mode&0o7777); got != want {
		t.Errorf("raw mode bits = %#o, want %#o", got, want)
	}

	// Owner, with the high halves.
	if got := uint32(ino.UIDHigh)<<16 | uint32(ino.UID); got != st.Uid {
		t.Errorf("uid = %d, want %d", got, st.Uid)
	}
	if got := uint32(ino.GIDHigh)<<16 | uint32(ino.GID); got != st.Gid {
		t.Errorf("gid = %d, want %d", got, st.Gid)
	}

	// Modification time, seconds.
	if got := int64(ino.Mtime); got != st.Mtim.Sec {
		t.Errorf("mtime = %d, want %d", got, st.Mtim.Sec)
	}

	// Hardlinks: every name of one source inode maps to one image inode.
	if prev, ok := srcInodes[st.Ino]; ok && fi.InodeNumber() != prev {
		t.Errorf("hardlink resolves to inode %d, sibling has %d", fi.InodeNumber(), prev)
	}
	srcInodes[st.Ino] = fi.InodeNumber()
	if !fi.IsDir() && uint64(ino.LinksCount) != uint64(st.Nlink) {
		t.Errorf("link count = %d, want %d", ino.LinksCount, st.Nlink)
	}

	switch {
	case srcInfo.Mode()&fs.ModeSymlink != 0:
		want, _ := os.Readlink(srcPath)
		got, err := fsys.ReadLink(rel)
		if err != nil {
			t.Fatalf("ReadLink: %v", err)
		}
		if got != want {
			t.Errorf("ReadLink = %q, want %q (verbatim)", got, want)
		}
		if fi.Size() != int64(len(want)) {
			t.Errorf("symlink size = %d, want %d", fi.Size(), len(want))
		}
		return // symlinks carry no xattrs here

	case srcInfo.IsDir():
		entries, err := os.ReadDir(srcPath)
		if err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, e := range entries {
			want = append(want, e.Name())
		}
		if rel == "special" { // device nodes added by debugfs
			want = append(want, "chr", "blk")
		}
		got, err := fsys.ReadDirNames(rel)
		if err != nil {
			t.Fatalf("ReadDirNames: %v", err)
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("ReadDirNames: %d names, want %d", len(got), len(want))
		}
		des, err := fsys.ReadDir(rel)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(des) != len(want) {
			t.Errorf("ReadDir: %d entries, want %d", len(des), len(want))
		}

	case srcInfo.Mode().IsRegular():
		if fi.Size() != srcInfo.Size() {
			t.Errorf("size = %d, want %d", fi.Size(), srcInfo.Size())
		}
		f, err := fsys.Open(rel)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer f.Close()
		if got, want := hashReader(t, f), hashFile(t, srcPath); got != want {
			t.Errorf("content differs from source")
		}
	}

	// Xattrs, as the kernel reports them on the source. Covers user.* and
	// the POSIX ACLs, which must come back in xattr format, not disk format.
	if srcInfo.Mode()&fs.ModeNamedPipe != 0 {
		return
	}
	want := sourceXattrs(t, srcPath)
	got, err := fsys.ListXattrs(fi.InodeNumber())
	if err != nil {
		t.Fatalf("ListXattrs: %v", err)
	}
	if rel == "xattr/small" { // plus the two set by debugfs
		want["security.test"] = []byte("sec-value")
		want["trusted.test"] = []byte("trusted-value")
	}
	if !tr.aclsOK {
		delete(want, XattrPosixACLAccess)
		delete(want, XattrPosixACLDefault)
	}
	compareXattrs(t, got, want)
}

func compareXattrs(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("xattrs: got %d %v, want %d %v", len(got), keys(got), len(want), keys(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("xattr %q missing", name)
			continue
		}
		if !bytes.Equal(g, w) {
			t.Errorf("xattr %q = %x, want %x", name, g, w)
		}
	}
}

func keys(m map[string][]byte) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// checkDebugfsEntries covers what the source could not hold.
func checkDebugfsEntries(t *testing.T, fsys *FileSystem, cfg imageConfig) {
	t.Run("uid-gid-above-65535", func(t *testing.T) {
		ino := lookupInfo(t, fsys, "owners/uid-100000").Inode()
		if uid := uint32(ino.UIDHigh)<<16 | uint32(ino.UID); uid != 100000 {
			t.Errorf("uid = %d, want 100000", uid)
		}
		if gid := uint32(ino.GIDHigh)<<16 | uint32(ino.GID); gid != 100000 {
			t.Errorf("gid = %d, want 100000", gid)
		}
		ino = lookupInfo(t, fsys, "owners/root").Inode()
		if ino.UID != 0 || ino.UIDHigh != 0 || ino.GID != 0 {
			t.Errorf("root-owned file reads as %d:%d", ino.UID, ino.GID)
		}
	})
	t.Run("device-nodes", func(t *testing.T) {
		chr := lookupInfo(t, fsys, "special/chr")
		if chr.Mode()&fs.ModeCharDevice == 0 || !chr.Inode().IsCharDevice() {
			t.Errorf("chr mode = %v", chr.Mode())
		}
		blk := lookupInfo(t, fsys, "special/blk")
		if blk.Mode()&fs.ModeDevice == 0 || blk.Mode()&fs.ModeCharDevice != 0 || !blk.Inode().IsBlockDevice() {
			t.Errorf("blk mode = %v", blk.Mode())
		}
		fifo := lookupInfo(t, fsys, "special/fifo")
		if fifo.Mode()&fs.ModeNamedPipe == 0 || !fifo.Inode().IsFifo() {
			t.Errorf("fifo mode = %v", fifo.Mode())
		}
	})
	t.Run("big-dir-is-htree", func(t *testing.T) {
		if !lookupInfo(t, fsys, "bigdir").Inode().UsesDirectoryHashTree() {
			t.Error("bigdir is not hash-tree indexed; the htree path is not exercised")
		}
		// A lookup deep inside the index.
		if _, err := fsys.ReadDirInfo("bigdir/entry-02999-" + strings.Repeat("x", 2999%40)); err != nil {
			t.Error(err)
		}
	})
	if strings.Contains(cfg.features, "extent") {
		t.Run("extent-tree-has-index-level", func(t *testing.T) {
			ino := lookupInfo(t, fsys, "sparse/fragmented.bin").Inode()
			if depth := binary.LittleEndian.Uint16(ino.BlockOrExtents[6:]); depth == 0 {
				t.Error("fragmented file has a depth-0 extent tree; the index path is not exercised")
			}
		})
	}
}

// ---------------------------------------------------------------- API behaviour

func smallImage(t *testing.T) (*FileSystem, string) {
	t.Helper()
	cfg := imageConfigs[0]
	tr := buildSourceTree(t, cfg, false)
	img := buildImage(t, cfg, tr.dir, tr.debugfs)
	return openImage(t, img, 0), img
}

func TestImagePathForms(t *testing.T) {
	fsys, _ := smallImage(t)
	// Open follows the io/fs path rules, plus an optional leading slash.
	for _, p := range []string{"links//target.txt", "./links/target.txt", "links/../links/target.txt"} {
		if _, err := fsys.Open(p); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) = %v, want fs.ErrInvalid", p, err)
		}
	}
	for _, p := range []string{"links/target.txt", "/links/target.txt"} {
		f, err := fsys.Open(p)
		if err != nil {
			t.Errorf("Open(%q): %v", p, err)
			continue
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != "link target\n" {
			t.Errorf("Open(%q) read %q", p, b)
		}
	}
	for _, p := range []string{"", "/", "."} {
		if fi, err := fsys.ReadDirInfo(p); err != nil || !fi.IsDir() || fi.(FileInfo).InodeNumber() != rootInodeNumber {
			t.Errorf("ReadDirInfo(%q) = %v, %v; want the root directory", p, fi, err)
		}
		if _, err := fsys.ReadDirNames(p); err != nil {
			t.Errorf("ReadDirNames(%q): %v", p, err)
		}
	}
}

func TestImageErrors(t *testing.T) {
	fsys, _ := smallImage(t)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Open missing", func() error { _, err := fsys.Open("nope"); return err }},
		{"Open missing parent", func() error { _, err := fsys.Open("nope/file"); return err }},
		{"Open through a file", func() error { _, err := fsys.Open("links/target.txt/x"); return err }},
		{"Open directory", func() error { _, err := fsys.Open("links"); return err }},
		{"Stat missing", func() error { _, err := fsys.Stat("nope"); return err }},
		{"ReadDir missing", func() error { _, err := fsys.ReadDir("nope"); return err }},
		{"ReadDir on file", func() error { _, err := fsys.ReadDir("links/target.txt"); return err }},
		{"ReadDirNames on file", func() error { _, err := fsys.ReadDirNames("links/target.txt"); return err }},
		{"ReadLink missing", func() error { _, err := fsys.ReadLink("nope"); return err }},
		{"ReadLink on file", func() error { _, err := fsys.ReadLink("links/target.txt"); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			if err == nil {
				t.Fatal("no error")
			}
			if c.name != "ReadLink on file" && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("error %v is not fs.ErrNotExist", err)
			}
		})
	}
	if _, err := fsys.ReadLink("links/target.txt"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ReadLink on a file: %v, want fs.ErrInvalid", err)
	}
}

// Symlinks are resolved the way the kernel resolves them: relative to the
// directory holding the link, absolute from the filesystem root.
func TestImageSymlinkResolution(t *testing.T) {
	fsys, _ := smallImage(t)
	read := func(p string) (string, error) {
		f, err := fsys.Open(p)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		return string(b), err
	}
	for link, want := range map[string]string{
		"links/fast-rel":       "link target\n",
		"links/sub/up-rel":     "link target\n",
		"links/sub/same-dir":   "inner\n",
		"links/sub/to-sibling": "inner\n",
		"links/abs":            "link target\n",
		"links/slow-rel":       "link target\n",
		"links/chain-1":        "link target\n",
	} {
		got, err := read(link)
		if err != nil || got != want {
			t.Errorf("Open(%q) = %q, %v; want %q", link, got, err, want)
		}
	}
	for _, link := range []string{"links/broken", "links/loop-a", "links/to-dir"} {
		if _, err := read(link); err == nil {
			t.Errorf("Open(%q) succeeded, want an error", link)
		}
	}
	if _, err := read("links/loop-a"); err != nil && !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Errorf("loop error = %v, want ELOOP", err)
	}
}

// Lstat describes the link itself; Stat follows it.
func TestImageLstatDoesNotFollow(t *testing.T) {
	fsys, _ := smallImage(t)
	for _, link := range []string{"links/fast-rel", "links/broken", "links/loop-a", "links/to-dir"} {
		fi, err := fsys.Lstat(link)
		if err != nil {
			t.Errorf("Lstat(%q): %v", link, err)
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("Lstat(%q).Mode() = %v, want a symlink", link, fi.Mode())
		}
	}
	fi, err := fsys.Stat("links/fast-rel")
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len("link target\n")) {
		t.Errorf("Stat(links/fast-rel) = %v, %v; want the target file", fi, err)
	}
}

// A filesystem inside a partition is read through a SectionReader at its
// offset; nothing may read outside it.
func TestImageAtOffset(t *testing.T) {
	_, img := smallImage(t)
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	const offset = 1 << 20
	shifted := filepath.Join(t.TempDir(), "partitioned.img")
	prefix := bytes.Repeat([]byte{0xA5}, offset)
	if err := os.WriteFile(shifted, append(prefix, data...), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys := openImage(t, shifted, offset)
	f, err := fsys.Open("hard/other/c")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if b, _ := io.ReadAll(f); len(b) != 5000 {
		t.Errorf("read %d bytes, want 5000", len(b))
	}
}

// The feature gate refuses layouts this reader cannot interpret.
func TestImageUnsupportedFeatures(t *testing.T) {
	requireTools(t, "mke2fs")
	cases := []struct {
		features string
		extra    []string
		want     string
	}{
		{"inline_data", nil, "inline_data"},
		{"bigalloc", []string{"-C", "16384"}, "bigalloc"},
		{"encrypt", nil, "encrypt"},
		{"casefold", nil, "casefold"},
		{"quota", nil, "quota"},
		{"project,quota", []string{"-I", "256"}, "project"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			img := filepath.Join(t.TempDir(), "fs.img")
			args := append([]string{"-q", "-F", "-t", "ext4", "-b", "4096",
				"-O", baseFeatures + ",extent,64bit,flex_bg,extra_isize," + c.features}, c.extra...)
			args = append(args, img, "64M")
			cmd := exec.Command("mke2fs", args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("this mke2fs cannot create %s: %v\n%s", c.features, err, out)
			}
			f, err := os.Open(img)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			st, _ := f.Stat()
			_, err = NewFS(*io.NewSectionReader(f, 0, st.Size()), nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("NewFS = %v, want an error naming %q", err, c.want)
			}
		})
	}
	// ea_inode is read (xattr values in separate inodes), so it opens.
	t.Run("ea_inode is supported", func(t *testing.T) {
		img := filepath.Join(t.TempDir(), "fs.img")
		cmd := exec.Command("mke2fs", "-q", "-F", "-t", "ext4", "-b", "4096",
			"-O", baseFeatures+",extent,64bit,flex_bg,extra_isize,ea_inode", img, "64M")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("this mke2fs cannot create ea_inode: %v\n%s", err, out)
		}
		f, err := os.Open(img)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		st, _ := f.Stat()
		if _, err := NewFS(*io.NewSectionReader(f, 0, st.Size()), nil); err != nil {
			t.Errorf("NewFS: %v", err)
		}
	})
}

// Damaged or foreign input fails with an error, never a panic.
func TestImageCorruptInput(t *testing.T) {
	_, img := smallImage(t)
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":           {},
		"zeros":           make([]byte, 1<<20),
		"superblock only": data[:2048],
		"bad magic": func() []byte {
			b := append([]byte(nil), data[:1<<20]...)
			b[1024+0x38] = 0
			return b
		}(),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			_, err := NewFS(*io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))), nil)
			if err == nil {
				t.Error("NewFS accepted it")
			}
		})
	}
	// Truncated after the metadata: opening may succeed, reading data fails.
	t.Run("truncated", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v", r)
			}
		}()
		b := data[:len(data)/4]
		fsys, err := NewFS(*io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))), nil)
		if err != nil {
			return
		}
		// Walk and read everything; errors are fine, panics are not.
		_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				_, _ = fsys.ReadLink(p)
				return nil
			}
			_, _ = fsys.ListXattrs(d.(dirEntry).InodeNumber())
			if f, err := fsys.Open(p); err == nil {
				_, _ = io.ReadAll(f)
			}
			return nil
		})
	})
}

// A damaged symlink inode with a negative or oversized size is refused with
// an error, before anything is sliced or allocated from it.
func TestImageCorruptSymlinkSize(t *testing.T) {
	cfg := imageConfigs[0]
	tr := buildSourceTree(t, cfg, false)
	img := buildImage(t, cfg, tr.dir, tr.debugfs)
	// Applied after buildImage, whose e2fsck pass would repair it.
	out := run(t, strings.Join([]string{
		"sif /links/fast-rel size 100000",        // larger than a block
		"sif /links/slow-rel size_hi 0x80000000", // negative as int64
	}, "\n")+"\n", "debugfs", "-w", "-f", "-", img)
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "debugfs") {
			t.Fatalf("debugfs reported: %q", line)
		}
	}
	fsys := openImage(t, img, 0)
	for _, link := range []string{"links/fast-rel", "links/slow-rel"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ReadLink(%q) panicked: %v", link, r)
				}
			}()
			if _, err := fsys.ReadLink(link); err == nil || !strings.Contains(err.Error(), "symlink size") {
				t.Errorf("ReadLink(%q) = %v, want a symlink size error", link, err)
			}
		}()
	}
}

// ---------------------------------------------------------------- EA inodes

// eaInodeConfig returns the named geometry from imageConfigs with ea_inode
// added: xattr values too large for an xattr block go to separate inodes.
func eaInodeConfig(t *testing.T, name string) imageConfig {
	t.Helper()
	for _, cfg := range imageConfigs {
		if cfg.name == name {
			cfg.name += "-ea_inode"
			cfg.features += ",ea_inode"
			return cfg
		}
	}
	t.Fatalf("no image config %q", name)
	return imageConfig{}
}

// eaInodeConfigNames are the geometries the EA inode tests run on: 4 KiB
// blocks with 512-byte inodes, 1 KiB blocks with 256-byte inodes, and a
// block-mapped (no extents) filesystem whose EA inodes use the classic
// block map, indirect block included.
var eaInodeConfigNames = []string{"skeleton-4k-i512", "ext4-1k-i256", "ext3-1k-blockmap"}

// shmSourceDir returns an empty directory under /dev/shm. The default temp
// directory's filesystem may refuse xattr values over about 4 KiB; tmpfs
// takes 64 KiB. Skips when /dev/shm cannot hold a 64 KiB user.* value.
func shmSourceDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/dev/shm", "ext4-ea-inode-")
	if err != nil {
		t.Skipf("no usable /dev/shm: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	probe := filepath.Join(dir, "probe")
	writeFile(t, probe, nil, 0o644)
	if err := syscall.Setxattr(probe, "user.probe", make([]byte, xattrValueMax), 0); err != nil {
		t.Skipf("/dev/shm does not take a 64 KiB user.* xattr: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	return dir
}

// buildEASourceTree creates files whose xattr values are large enough to be
// stored in EA inodes. Returns the tree root.
func buildEASourceTree(t *testing.T) string {
	t.Helper()
	requireTools(t, "mke2fs", "debugfs", "e2fsck")
	src := shmSourceDir(t)
	rng := rand.New(rand.NewSource(2))
	setx := func(p, name string, v []byte) {
		if err := syscall.Setxattr(filepath.Join(src, p), name, v, 0); err != nil {
			t.Fatalf("setxattr %s %s (%d bytes): %v", p, name, len(v), err)
		}
	}
	file := func(p string) { writeFile(t, filepath.Join(src, p), []byte(p), 0o644) }

	// One value each, either side of what fits in a 4 KiB xattr block.
	for _, n := range []int{4040, 4041, 8192, 65536} {
		p := fmt.Sprintf("v%05d", n)
		file(p)
		setx(p, "user.v", randBytes(rng, n))
	}
	// Two maximum-size values on one inode.
	file("two-max")
	setx("two-max", "user.a", randBytes(rng, 65536))
	setx("two-max", "user.b", randBytes(rng, 65536))
	// Inline, xattr block and EA inode on one inode.
	file("mixed")
	setx("mixed", "user.inline", randBytes(rng, 10))
	setx("mixed", "user.block", randBytes(rng, 3000))
	setx("mixed", "user.eainode", randBytes(rng, 16384))
	// An ACL beside a large value, when the source takes ACLs.
	file("acl")
	access := posixACL(
		[3]uint32{aclUserObj, 6, undef}, [3]uint32{aclUser, 6, 1234},
		[3]uint32{aclGroupObj, 4, undef}, [3]uint32{aclMask, 6, undef},
		[3]uint32{aclOther, 4, undef})
	if err := syscall.Setxattr(filepath.Join(src, "acl"), XattrPosixACLAccess, access, 0); err != nil {
		t.Logf("source does not take ACLs (%v); acl file has only user.v", err)
	}
	setx("acl", "user.v", randBytes(rng, 8192))
	// A directory with a large value.
	if err := os.Mkdir(filepath.Join(src, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	setx("dir", "user.v", randBytes(rng, 8192))
	// Two files and two names on one file with different values of the same
	// size, for the shared EA inode test to point at one another.
	file("shared-a")
	setx("shared-a", "user.v", randBytes(rng, 8192))
	file("shared-b")
	setx("shared-b", "user.v", randBytes(rng, 8192))
	return src
}

var debugfsStatHead = regexp.MustCompile(`^Inode: (\d+)\s+Type: (\S+).*Flags: 0x([0-9a-f]+)`)
var debugfsStatSize = regexp.MustCompile(`\bSize: (\d+)`)

// eaInodeSizes lists, through debugfs, the size of every regular inode that
// carries EXT4_EA_INODE_FL among the first n inodes.
func eaInodeSizes(t *testing.T, img string, n int) map[int]int64 {
	t.Helper()
	var cmds []string
	for i := 1; i <= n; i++ {
		cmds = append(cmds, fmt.Sprintf("stat <%d>", i))
	}
	out := run(t, strings.Join(cmds, "\n")+"\n", "debugfs", "-f", "-", img)
	sizes := map[int]int64{}
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		m := debugfsStatHead.FindStringSubmatch(line)
		if m == nil || m[2] != "regular" || i+2 >= len(lines) {
			continue
		}
		flags, _ := strconv.ParseUint(m[3], 16, 32)
		if flags&0x200000 == 0 {
			continue
		}
		// "Generation: ..." then "User: ... Size: N".
		s := debugfsStatSize.FindStringSubmatch(lines[i+2])
		if s == nil {
			t.Fatalf("no size after %q: %q", line, lines[i+2])
		}
		ino, _ := strconv.Atoi(m[1])
		sizes[ino], _ = strconv.ParseInt(s[1], 10, 64)
	}
	return sizes
}

// buildEAImage builds an ea_inode image from src and checks with debugfs
// that the 4041-byte value -- one byte more than fits beside its entry in a
// 4 KiB xattr block -- really sits in an EA inode. Skips, naming the reason,
// when this mke2fs does not put large values in EA inodes.
func buildEAImage(t *testing.T, cfg imageConfig, src string) string {
	t.Helper()
	img := buildImage(t, cfg, src, nil)
	sizes := eaInodeSizes(t, img, 128)
	var found []int64
	has4041 := false
	for _, s := range sizes {
		found = append(found, s)
		has4041 = has4041 || s == 4041
	}
	sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
	t.Logf("%s: EA inode sizes %v", cfg.name, found)
	if !has4041 {
		if os.Getenv("EXT4_REQUIRE_TOOLS") != "" {
			t.Fatalf("mke2fs -d put no 4041-byte value in an EA inode (EA inodes: %v)", found)
		}
		t.Skipf("this mke2fs -d does not store large values in EA inodes (EA inodes: %v)", found)
	}
	return img
}

// Values stored in EA inodes read back as the source holds them, beside
// inline and block values, on files and directories, for every geometry.
func TestImageEAInodeXattrs(t *testing.T) {
	for _, name := range eaInodeConfigNames {
		cfg := eaInodeConfig(t, name)
		t.Run(cfg.name, func(t *testing.T) {
			src := buildEASourceTree(t)
			img := buildEAImage(t, cfg, src)
			fsys := openImage(t, img, 0)
			entries, err := os.ReadDir(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				rel := e.Name()
				t.Run(rel, func(t *testing.T) {
					want := sourceXattrs(t, filepath.Join(src, rel))
					got, err := fsys.ListXattrs(lookupInfo(t, fsys, rel).InodeNumber())
					if err != nil {
						t.Fatalf("ListXattrs: %v", err)
					}
					compareXattrs(t, got, want)
				})
			}
		})
	}
}

// eaEntry locates an xattr entry on the inode at path in an image, looking
// first in the inode body and then in its xattr block. Returns the entry's
// byte offset in the image and its e_value_inum. Found through debugfs imap,
// so it works on an image this package cannot open.
func eaEntry(t *testing.T, img string, data []byte, cfg imageConfig, path string, index byte, name string) (int64, uint32) {
	t.Helper()
	out := run(t, "", "debugfs", "-R", "imap "+path, img)
	m := regexp.MustCompile(`located at block (\d+), offset 0x([0-9a-f]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("debugfs imap %s: %q", path, out)
	}
	blk, _ := strconv.ParseInt(m[1], 10, 64)
	off, _ := strconv.ParseInt(m[2], 16, 64)
	inodeAt := blk*int64(cfg.blockSize) + off
	raw := data[inodeAt : inodeAt+int64(cfg.inodeSize)]

	walk := func(pos, end int64) (int64, uint32, bool) {
		for pos+xattrEntrySize <= end && binary.LittleEndian.Uint32(data[pos:]) != 0 {
			nl := int64(data[pos])
			if data[pos+1] == index && string(data[pos+xattrEntrySize:pos+xattrEntrySize+nl]) == name {
				return pos, binary.LittleEndian.Uint32(data[pos+4:]), true
			}
			pos += int64(entryLen(int(nl)))
		}
		return 0, 0, false
	}
	if cfg.inodeSize > goodOldInodeSize {
		magicAt := int64(goodOldInodeSize) + int64(binary.LittleEndian.Uint16(raw[extraIsizeOffset:]))
		if binary.LittleEndian.Uint32(raw[magicAt:]) == xattrMagic {
			if pos, inum, ok := walk(inodeAt+magicAt+4, inodeAt+int64(cfg.inodeSize)); ok {
				return pos, inum
			}
		}
	}
	if acl := int64(binary.LittleEndian.Uint32(raw[104:])) | int64(binary.LittleEndian.Uint16(raw[118:]))<<32; acl != 0 {
		base := acl * int64(cfg.blockSize)
		if pos, inum, ok := walk(base+xattrHeaderSize, base+int64(cfg.blockSize)); ok {
			return pos, inum
		}
	}
	t.Fatalf("%s: no xattr entry %d/%q", path, index, name)
	return 0, 0
}

// writePatched writes data to a new image file and returns its path.
func writePatched(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "patched.img")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// debugfsPatch applies debugfs write commands to a copy of img.
func debugfsPatch(t *testing.T, img string, cmds ...string) string {
	t.Helper()
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	p := writePatched(t, data)
	out := run(t, strings.Join(cmds, "\n")+"\n", "debugfs", "-w", "-f", "-", p)
	for _, line := range strings.Split(out, "\n") {
		if line != "" && !strings.HasPrefix(line, "debugfs") {
			t.Fatalf("debugfs reported: %q\nfull output:\n%s", line, out)
		}
	}
	return p
}

// Several entries may point at one EA inode (the kernel shares EA inodes
// between equal values). mke2fs -d never shares one, so the image is
// patched: user.b of two-max is pointed at the EA inode of user.a, and
// user.v of shared-b at the EA inode of shared-a. Every value read must be
// its own memory: changing one returned value changes no other.
func TestImageEAInodeShared(t *testing.T) {
	for _, name := range eaInodeConfigNames {
		cfg := eaInodeConfig(t, name)
		t.Run(cfg.name, func(t *testing.T) {
			src := buildEASourceTree(t)
			img := buildEAImage(t, cfg, src)
			data, err := os.ReadFile(img)
			if err != nil {
				t.Fatal(err)
			}
			_, aInum := eaEntry(t, img, data, cfg, "/two-max", 1, "a")
			bAt, bInum := eaEntry(t, img, data, cfg, "/two-max", 1, "b")
			_, saInum := eaEntry(t, img, data, cfg, "/shared-a", 1, "v")
			sbAt, sbInum := eaEntry(t, img, data, cfg, "/shared-b", 1, "v")
			if aInum == 0 || saInum == 0 || bInum == 0 || sbInum == 0 {
				t.Fatalf("values not in EA inodes: a=%d b=%d shared-a=%d shared-b=%d", aInum, bInum, saInum, sbInum)
			}
			binary.LittleEndian.PutUint32(data[bAt+4:], aInum)
			binary.LittleEndian.PutUint32(data[sbAt+4:], saInum)
			fsys := openImage(t, writePatched(t, data), 0)

			wantA := sourceXattrs(t, filepath.Join(src, "two-max"))["user.a"]
			wantSA := sourceXattrs(t, filepath.Join(src, "shared-a"))["user.v"]

			// Two names on one inode, one EA inode.
			got, err := fsys.ListXattrs(lookupInfo(t, fsys, "two-max").InodeNumber())
			if err != nil {
				t.Fatalf("ListXattrs(two-max): %v", err)
			}
			compareXattrs(t, got, map[string][]byte{"user.a": wantA, "user.b": wantA})
			got["user.a"][0] ^= 0xff
			if !bytes.Equal(got["user.b"], wantA) {
				t.Error("changing user.a changed user.b: the values share memory")
			}

			// Two files, one EA inode.
			sa, err := fsys.ListXattrs(lookupInfo(t, fsys, "shared-a").InodeNumber())
			if err != nil {
				t.Fatalf("ListXattrs(shared-a): %v", err)
			}
			compareXattrs(t, sa, map[string][]byte{"user.v": wantSA})
			sa["user.v"][0] ^= 0xff
			sb, err := fsys.ListXattrs(lookupInfo(t, fsys, "shared-b").InodeNumber())
			if err != nil {
				t.Fatalf("ListXattrs(shared-b): %v", err)
			}
			compareXattrs(t, sb, map[string][]byte{"user.v": wantSA})
			sa2, err := fsys.ListXattrs(lookupInfo(t, fsys, "shared-a").InodeNumber())
			if err != nil {
				t.Fatalf("ListXattrs(shared-a) again: %v", err)
			}
			compareXattrs(t, sa2, map[string][]byte{"user.v": wantSA})
		})
	}
}

// A damaged pointer to an EA inode, or a damaged EA inode, is an error
// naming the reason, never a panic and never bytes from elsewhere.
func TestImageEAInodeCorrupt(t *testing.T) {
	for _, name := range eaInodeConfigNames {
		cfg := eaInodeConfig(t, name)
		t.Run(cfg.name, func(t *testing.T) {
			src := buildEASourceTree(t)
			img := buildEAImage(t, cfg, src)
			data, err := os.ReadFile(img)
			if err != nil {
				t.Fatal(err)
			}
			entryAt, ea := eaEntry(t, img, data, cfg, "/v08192", 1, "v")
			if ea == 0 {
				t.Fatal("v08192 value is not in an EA inode")
			}
			plain := inodeNumber(t, img, "/v04040")
			dir := inodeNumber(t, img, "/dir")
			var sb Superblock
			if sb, err = parseSuperBlock(bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}

			pointAt := func(inum uint32) string {
				b := append([]byte(nil), data...)
				binary.LittleEndian.PutUint32(b[entryAt+4:], inum)
				return writePatched(t, b)
			}
			extents := strings.Contains(cfg.features, "extent")
			hole, outside := fmt.Sprintf("sif <%d> block[0] 0", ea), fmt.Sprintf("sif <%d> block[0] 0x7fffffff", ea)
			if extents {
				// i_block[0] is the extent header's magic and entry count:
				// no entries leaves the value a hole. i_block[5] is the
				// first extent's physical start.
				hole = fmt.Sprintf("sif <%d> block[0] 0xF30A", ea)
				outside = fmt.Sprintf("sif <%d> block[5] 0x7fffffff", ea)
			}
			cases := []struct {
				name, img, want string
			}{
				{"past the inode count", pointAt(sb.InodeCount + 1), "out of range"},
				{"inode 2", pointAt(2), "out of range"},
				{"last reserved inode", pointAt(sb.FirstIno - 1), "out of range"},
				{"directory", pointAt(uint32(dir)), "not a regular file"},
				{"regular file without EA_INODE_FL", pointAt(uint32(plain)), "EA_INODE_FL"},
				{"size one byte short", debugfsPatch(t, img, fmt.Sprintf("sif <%d> size 8191", ea)), "size 8191"},
				{"first extent removed (hole)", debugfsPatch(t, img, hole), "hole"},
				{"block outside the filesystem", debugfsPatch(t, img, outside), "outside the filesystem"},
			}
			for _, c := range cases {
				t.Run(c.name, func(t *testing.T) {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("panic: %v", r)
						}
					}()
					fsys := openImage(t, c.img, 0)
					got, err := fsys.ListXattrs(lookupInfo(t, fsys, "v08192").InodeNumber())
					if err == nil {
						t.Fatalf("ListXattrs succeeded with %d values", len(got))
					}
					if !strings.Contains(err.Error(), c.want) {
						t.Errorf("error %q does not mention %q", err, c.want)
					}
					t.Logf("%v", err)
				})
			}
		})
	}
}

// inodeNumber returns the inode number of path, through debugfs.
func inodeNumber(t *testing.T, img, path string) int {
	t.Helper()
	out := run(t, "", "debugfs", "-R", "stat "+path, img)
	var m []string
	if i := strings.Index(out, "Inode:"); i >= 0 {
		m = debugfsStatHead.FindStringSubmatch(out[i:])
	}
	if m == nil {
		t.Fatalf("debugfs stat %s: %q", path, out)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// A short symlink that owns an EA inode is still a fast symlink. The blocks
// of a link's EA inodes are charged to its i_blocks (e2fsck requires it, the
// kernel does it, and so does the backup side), so i_blocks counts more than
// an xattr block although the target sits in the inode. debugfs ea_set gives
// the links their values (an unprivileged user cannot set trusted.* in the
// source tree, and user.* is not allowed on a symlink).
//
// short-ea-block (xattr block and EA inode) and long-ea are charged by
// buildImage's e2fsck pass and come out clean. short-ea (EA inode, no xattr
// block) is given its value and its charge after that pass: e2fsck 1.46.5
// asks for the charge but, once it is made, takes the link for a slow one
// (ext2fs_inode_has_valid_blocks2 trusts i_blocks when there is no xattr
// block) and reports the target bytes as illegal blocks.
func TestImageSymlinkWithEAInode(t *testing.T) {
	for _, name := range eaInodeConfigNames {
		cfg := eaInodeConfig(t, name)
		t.Run(cfg.name, func(t *testing.T) {
			requireTools(t, "mke2fs", "debugfs", "e2fsck")
			rng := rand.New(rand.NewSource(3))
			src, vals := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(src, "target"), []byte("link target\n"), 0o644)
			longTarget := strings.Repeat("./", 40) + "target" // 86 bytes: a slow link
			for link, target := range map[string]string{
				"short-ea":       "target",
				"short-ea-block": "target",
				"long-ea":        longTarget,
			} {
				if err := os.Symlink(target, filepath.Join(src, link)); err != nil {
					t.Fatal(err)
				}
			}
			// A value as large as a block does not fit in an xattr block
			// and goes to an EA inode. ea_set -f in e2fsprogs 1.46.5 stores
			// the first block of the file.
			big := randBytes(rng, 4096)
			medium := randBytes(rng, 300)
			writeFile(t, filepath.Join(vals, "big"), big, 0o644)
			writeFile(t, filepath.Join(vals, "medium"), medium, 0o644)
			cmds := []string{
				"ea_set -f " + filepath.Join(vals, "big") + " /short-ea-block trusted.big",
				"ea_set -f " + filepath.Join(vals, "big") + " /long-ea trusted.big",
			}
			// Enough medium values to spill out of the inode into an
			// xattr block.
			for i := 1; i <= 4; i++ {
				cmds = append(cmds, fmt.Sprintf("ea_set -f %s /short-ea-block trusted.m%d",
					filepath.Join(vals, "medium"), i))
			}
			img := buildImage(t, cfg, src, cmds)
			// The stored value is one block (see above): one block charged.
			img = debugfsPatch(t, img,
				"ea_set -f "+filepath.Join(vals, "big")+" /short-ea trusted.big",
				fmt.Sprintf("sif /short-ea blocks %d", cfg.blockSize/512))
			fsys := openImage(t, img, 0)

			for link, target := range map[string]string{
				"short-ea":       "target",
				"short-ea-block": "target",
				"long-ea":        longTarget,
			} {
				t.Run(link, func(t *testing.T) {
					fi := lookupInfo(t, fsys, link)
					inode := fi.inode
					blocks := uint64(inode.BlocksHigh)<<32 | uint64(inode.BlocksLo)
					var xattrBlock uint64
					if inode.FileACLLo != 0 || inode.FileACLHigh != 0 {
						xattrBlock = uint64(cfg.blockSize / 512)
					}
					t.Logf("size %d, i_blocks %d, xattr block %v", inode.GetSize(), blocks, xattrBlock != 0)
					// The case under test: i_blocks alone would call a
					// short link slow.
					if blocks <= xattrBlock {
						t.Fatalf("i_blocks %d does not exceed the xattr block (%d): no EA inode blocks charged",
							blocks, xattrBlock)
					}
					if wantBlock := link == "short-ea-block"; (xattrBlock != 0) != wantBlock {
						t.Fatalf("%s: xattr block present = %v, want %v", link, xattrBlock != 0, wantBlock)
					}

					got, err := fsys.ReadLink(link)
					if err != nil || got != target {
						t.Fatalf("ReadLink = %q, %v; want %q", got, err, target)
					}
					f, err := fsys.Open(link)
					if err != nil {
						t.Fatalf("Open through the link: %v", err)
					}
					defer f.Close()
					if b, err := io.ReadAll(f); err != nil || string(b) != "link target\n" {
						t.Errorf("read through the link = %q, %v", b, err)
					}

					xattrs, err := fsys.ListXattrs(fi.InodeNumber())
					if err != nil {
						t.Fatalf("ListXattrs: %v", err)
					}
					v := xattrs["trusted.big"]
					if len(v) != cfg.blockSize || !bytes.Equal(v, big[:len(v)]) {
						t.Errorf("trusted.big: %d bytes, want the first %d bytes of the value", len(v), cfg.blockSize)
					}
					if link == "short-ea-block" {
						for i := 1; i <= 4; i++ {
							if m := xattrs[fmt.Sprintf("trusted.m%d", i)]; !bytes.Equal(m, medium) {
								t.Errorf("trusted.m%d: %d bytes, want the 300-byte value", i, len(m))
							}
						}
					}
				})
			}
		})
	}
}

// withSlack returns a copy of the image at img with n bytes appended, every
// 32-bit word of them holding ptr. A device usually extends past the
// filesystem (partition slack, the rest of a disk), so an address past the
// last block often reads successfully. Filled with a valid block number, the
// slack also reads as a plausible pointer table.
func withSlack(t *testing.T, img string, n int, ptr uint32) string {
	t.Helper()
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	slack := make([]byte, n)
	for i := 0; i+4 <= n; i += 4 {
		binary.LittleEndian.PutUint32(slack[i:], ptr)
	}
	return writePatched(t, append(data, slack...))
}

// openAndRead opens p and reads it to the end; the error is from either step.
func openAndRead(fsys *FileSystem, p string) ([]byte, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// An indirect block pointer past the end of the filesystem is an error, for
// a file and for an EA inode, even when the device has readable bytes there:
// those bytes are not a pointer table of this filesystem.
func TestImageIndirectPointerOutside(t *testing.T) {
	cfg := eaInodeConfig(t, "ext3-1k-blockmap")
	t.Run("file", func(t *testing.T) {
		src := t.TempDir()
		rng := rand.New(rand.NewSource(4))
		small := randBytes(rng, 20<<10) // 12 direct + 8 through the single indirect block
		big := randBytes(rng, 300<<10)  // reaches the double indirect block
		writeFile(t, filepath.Join(src, "small"), small, 0o644)
		writeFile(t, filepath.Join(src, "big"), big, 0o644)
		img := buildImage(t, cfg, src, nil)
		data, err := os.ReadFile(img)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := parseSuperBlock(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		past := sb.GetBlockCount() // first block past the filesystem

		// Unpatched, both read back whole.
		fsys := openImage(t, img, 0)
		for p, want := range map[string][]byte{"small": small, "big": big} {
			if got, err := openAndRead(fsys, p); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s unpatched: %d bytes, %v", p, len(got), err)
			}
		}

		cases := []struct{ name, path, field string }{
			{"single indirect", "small", "block[IND]"},
			{"single indirect of a larger file", "big", "block[IND]"},
			{"double indirect", "big", "block[DIND]"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				patched := debugfsPatch(t, img, fmt.Sprintf("sif /%s %s %d", c.path, c.field, past))
				fsys := openImage(t, withSlack(t, patched, 64<<10, sb.FirstDataBlock+1), 0)
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panic: %v", r)
					}
				}()
				got, err := openAndRead(fsys, c.path)
				if err == nil {
					t.Fatalf("read %d bytes, want an error", len(got))
				}
				if !strings.Contains(err.Error(), "outside the filesystem") {
					t.Errorf("error %q does not mention the filesystem bounds", err)
				}
			})
		}
	})
	t.Run("EA inode", func(t *testing.T) {
		src := buildEASourceTree(t)
		img := buildEAImage(t, cfg, src)
		data, err := os.ReadFile(img)
		if err != nil {
			t.Fatal(err)
		}
		// 64 KiB on 1 KiB blocks: the single indirect block maps most of it.
		_, ea := eaEntry(t, img, data, cfg, "/v65536", 1, "v")
		if ea == 0 {
			t.Fatal("v65536 value is not in an EA inode")
		}
		sb, err := parseSuperBlock(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		patched := debugfsPatch(t, img, fmt.Sprintf("sif <%d> block[IND] %d", ea, sb.GetBlockCount()))
		fsys := openImage(t, withSlack(t, patched, 64<<10, sb.FirstDataBlock+1), 0)
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v", r)
			}
		}()
		got, err := fsys.ListXattrs(lookupInfo(t, fsys, "v65536").InodeNumber())
		if err == nil {
			t.Fatalf("ListXattrs succeeded with %d values", len(got))
		}
		if !strings.Contains(err.Error(), "outside the filesystem") {
			t.Errorf("error %q does not mention the filesystem bounds", err)
		}
	})
}

// A block-map data block past the end of the filesystem is an error, not
// the device's slack served as file content: a direct block of a file, and
// one named by a directory's block map.
func TestImageBlockMapDataOutside(t *testing.T) {
	cfg := eaInodeConfig(t, "ext3-1k-blockmap")
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "small"), bytes.Repeat([]byte("data"), 5<<10), 0o644)
	if err := os.MkdirAll(filepath.Join(src, "dir", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	img := buildImage(t, cfg, src, nil)
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := parseSuperBlock(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	past := sb.GetBlockCount()
	cases := []struct {
		name, patch string
		read        func(*FileSystem) error
	}{
		{"file direct block", fmt.Sprintf("sif /small block[0] %d", past), func(fsys *FileSystem) error {
			b, err := openAndRead(fsys, "small")
			if err == nil && len(b) >= 8 {
				t.Logf("read %d bytes, first %q", len(b), b[:8])
			}
			return err
		}},
		{"directory block", fmt.Sprintf("sif /dir block[0] %d", past), func(fsys *FileSystem) error {
			_, err := fsys.ReadDir("dir")
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The slack holds a copy of the image's start: plausible bytes.
			patched := debugfsPatch(t, img, c.patch)
			pd, err := os.ReadFile(patched)
			if err != nil {
				t.Fatal(err)
			}
			fsys := openImage(t, writePatched(t, append(pd, pd[:64<<10]...)), 0)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			err = c.read(fsys)
			if err == nil {
				t.Fatal("succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "outside the filesystem") {
				t.Errorf("error %q does not mention the filesystem bounds", err)
			}
		})
	}
}
