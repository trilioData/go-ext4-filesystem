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
	"sort"
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
		if cfg.features == imageConfigs[4].features {
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
		{"metadata_csum,metadata_csum_seed", nil, "metadata_csum_seed"},
		{"encrypt", nil, "encrypt"},
		{"casefold", nil, "casefold"},
		{"ea_inode", nil, "ea_inode"},
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
		_ = filepath.WalkDir(".", func(string, fs.DirEntry, error) error { return nil })
		if f, err := fsys.Open("sizes/f1048576"); err == nil {
			_, _ = io.ReadAll(f)
		}
	})
}
