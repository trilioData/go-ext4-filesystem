package ext4

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func TestExt4ACLToXattr(t *testing.T) {
	tests := []struct {
		name    string
		disk    string
		want    string
		wantErr string
	}{
		{
			// setfacl -m u:1234:rw on a 0644 file. The disk bytes are what
			// libext2fs wrote; the xattr bytes are what getxattr(2) returned
			// for the same file on the mounted source.
			name: "user entry with mask",
			disk: "01000000 0100 0600 0200 0600 d2040000 0400 0400 1000 0600 2000 0400",
			want: "02000000 0100 0600 ffffffff 0200 0600 d2040000 0400 0400 ffffffff" +
				" 1000 0600 ffffffff 2000 0400 ffffffff",
		},
		{
			name: "group entry",
			disk: "01000000 0100 0700 0400 0500 0800 0500 39300000 1000 0700 2000 0000",
			want: "02000000 0100 0700 ffffffff 0400 0500 ffffffff 0800 0500 39300000" +
				" 1000 0700 ffffffff 2000 0000 ffffffff",
		},
		{
			name: "minimal three-entry ACL",
			disk: "01000000 0100 0600 0400 0400 2000 0400",
			want: "02000000 0100 0600 ffffffff 0400 0400 ffffffff 2000 0400 ffffffff",
		},
		{
			name: "header only",
			disk: "01000000",
			want: "02000000",
		},
		{
			name: "already xattr format is passed through",
			disk: "02000000 0100 0600 ffffffff 2000 0400 ffffffff",
			want: "02000000 0100 0600 ffffffff 2000 0400 ffffffff",
		},
		{name: "empty", disk: "", wantErr: "shorter than its header"},
		{name: "short header", disk: "0100", wantErr: "shorter than its header"},
		{name: "unknown version", disk: "03000000 0100 0600", wantErr: "unknown ACL version 3"},
		{name: "truncated short entry", disk: "01000000 0100", wantErr: "truncated"},
		{name: "truncated long entry", disk: "01000000 0200 0600 d204", wantErr: "truncated"},
		{name: "unknown tag", disk: "01000000 4000 0600", wantErr: "unknown ACL tag 0x40"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ext4ACLToXattr(unhex(t, tt.disk))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if want := unhex(t, tt.want); !bytes.Equal(got, want) {
				t.Fatalf("got  %x\nwant %x", got, want)
			}
		})
	}
}

func TestExt4ACLToXattrDoesNotAliasInput(t *testing.T) {
	in := unhex(t, "02000000 0100 0600 ffffffff")
	out, err := ext4ACLToXattr(in)
	if err != nil {
		t.Fatal(err)
	}
	out[4] = 0xEE
	if in[4] == 0xEE {
		t.Fatal("pass-through result shares memory with the input")
	}
}

func TestIsPosixACLXattr(t *testing.T) {
	for name, want := range map[string]bool{
		"system.posix_acl_access":  true,
		"system.posix_acl_default": true,
		"system.richacl":           false,
		"user.posix_acl_access":    false,
		"trusted.SGI_ACL_FILE":     false,
	} {
		if got := isPosixACLXattr(name); got != want {
			t.Errorf("isPosixACLXattr(%q) = %v, want %v", name, got, want)
		}
	}
}
