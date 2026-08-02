//go:build !windows

package staging

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRejectsForeignUnixOwnerWhenPrivileged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires a privileged Unix test process")
	}
	root := filepath.Join(t.TempDir(), "foreign-owner")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, 1, -1); err != nil {
		t.Skipf("cannot create a foreign-owned directory: %v", err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("foreign-owned staging root accepted")
	}
}
