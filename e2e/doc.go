// Package e2e holds the end-to-end clone test: a real ext4 disk, formatted
// with mkfs.ext4 and populated through a kernel mount, is read with this
// library and written out as a copy, which is then compared with the mounted
// original. It needs root and is built only with the e2e tag; see
// scripts/e2e-clone.sh.
package e2e
