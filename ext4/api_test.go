//go:build linux

package ext4

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"reflect"
	"testing"
)

// Accessors and small public helpers, against a real image.
func TestImageAccessors(t *testing.T) {
	fsys, img := smallImage(t)

	t.Run("Check", func(t *testing.T) {
		f, err := os.Open(img)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if !Check(f) {
			t.Error("Check rejected a valid image")
		}
		if Check(bytes.NewReader(make([]byte, 4096))) {
			t.Error("Check accepted zeros")
		}
	})

	t.Run("GetSuperBlock", func(t *testing.T) {
		sb := fsys.GetSuperBlock()
		if sb.GetBlockSize() != 4096 || sb.InodeSize != 512 {
			t.Errorf("block size %d, inode size %d; want 4096, 512", sb.GetBlockSize(), sb.InodeSize)
		}
		if sb.UnsupportedCompatFeatures() != 0 {
			t.Errorf("unexpected unsupported COMPAT bits %#x", sb.UnsupportedCompatFeatures())
		}
	})

	t.Run("DirEntry", func(t *testing.T) {
		des, err := fsys.ReadDir("links")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]fs.FileMode{}
		for _, de := range des {
			info, err := de.Info()
			if err != nil {
				t.Fatal(err)
			}
			if info.Name() != de.Name() {
				t.Errorf("Info().Name() = %q, entry name %q", info.Name(), de.Name())
			}
			seen[de.Name()] = de.Type()
		}
		if seen["target.txt"] != 0 || seen["sub"] != fs.ModeDir || seen["fast-rel"] != fs.ModeSymlink {
			t.Errorf("entry types = %v", seen)
		}
	})

	t.Run("FileInfo", func(t *testing.T) {
		fi := lookupInfo(t, fsys, "links/fast-rel")
		if !fi.IsSymlink() {
			t.Error("IsSymlink() = false for a symlink")
		}
		if fi.Sys() != fi.Inode() {
			t.Error("Sys() does not return the inode")
		}
		fi = lookupInfo(t, fsys, "times/y2000")
		if got := fi.ModTime().Unix(); got != 946684800 {
			t.Errorf("ModTime() = %d, want 946684800", got)
		}
	})

	t.Run("File", func(t *testing.T) {
		f, err := fsys.Open("links/sub/inner.txt")
		if err != nil {
			t.Fatal(err)
		}
		file := f.(*File)
		if file.Dir() != "links/sub/" || file.FilePath() != "links/sub/inner.txt" {
			t.Errorf("Dir() = %q, FilePath() = %q", file.Dir(), file.FilePath())
		}
		st, err := f.Stat()
		if err != nil || st.Size() != int64(len("inner\n")) || st.Name() != "inner.txt" {
			t.Errorf("Stat() = %v, %v", st, err)
		}
		b, err := io.ReadAll(f)
		if err != nil || string(b) != "inner\n" {
			t.Errorf("read %q, %v", b, err)
		}
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})

	t.Run("XattrNames", func(t *testing.T) {
		names, err := fsys.XattrNames(lookupInfo(t, fsys, "xattr/small").InodeNumber())
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"security.test", "trusted.test", "user.a"}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("XattrNames = %v, want %v", names, want)
		}
		names, err = fsys.XattrNames(lookupInfo(t, fsys, "links/target.txt").InodeNumber())
		if err != nil || len(names) != 0 {
			t.Errorf("XattrNames on a plain file = %v, %v", names, err)
		}
	})

	t.Run("ListXattrs bad inode", func(t *testing.T) {
		if _, err := fsys.ListXattrs(1 << 40); err == nil {
			t.Error("no error for an inode outside the filesystem")
		}
	})
}

func TestDirEntryCacheEviction(t *testing.T) {
	entries := func(n int) []DirectoryEntry2 { return make([]DirectoryEntry2, n) }

	c := newDirEntryCache(10)
	c.put(1, entries(4))
	c.put(2, entries(4))
	if _, ok := c.get(1); !ok { // 1 becomes most recently used
		t.Fatal("1 missing")
	}
	c.put(3, entries(4)) // over budget: evicts 2, the least recently used
	if _, ok := c.get(2); ok {
		t.Error("2 was not evicted")
	}
	for _, ino := range []int64{1, 3} {
		if _, ok := c.get(ino); !ok {
			t.Errorf("%d evicted", ino)
		}
	}
	if c.curEntries != 8 {
		t.Errorf("curEntries = %d, want 8", c.curEntries)
	}

	// Replacing an entry adjusts the count instead of adding to it.
	c.put(1, entries(2))
	if c.curEntries != 6 {
		t.Errorf("after replace curEntries = %d, want 6", c.curEntries)
	}

	// One directory larger than the whole budget is kept.
	c.put(4, entries(50))
	if _, ok := c.get(4); !ok || c.order.Len() != 1 {
		t.Errorf("oversized directory: present=%v, len=%d", ok, c.order.Len())
	}

	// A nil cache is a no-op, and a non-positive budget becomes 1.
	var nilCache *dirEntryCache
	nilCache.put(1, entries(1))
	if _, ok := nilCache.get(1); ok {
		t.Error("nil cache returned an entry")
	}
	if newDirEntryCache(0).maxEntries != 1 {
		t.Error("zero budget not clamped to 1")
	}
}
