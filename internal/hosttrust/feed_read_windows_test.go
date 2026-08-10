//go:build windows

package hosttrust

import (
	"io/fs"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsFeedReadTransientErrorsAreNarrow(t *testing.T) {
	for _, err := range []error{
		&os.PathError{Op: "open", Path: "current.json", Err: windows.ERROR_SHARING_VIOLATION},
		&os.PathError{Op: "open", Path: "current.json", Err: windows.ERROR_LOCK_VIOLATION},
		&os.PathError{Op: "open", Path: "current.json", Err: fs.ErrNotExist},
	} {
		if !isTransientFeedReadError(err) {
			t.Fatalf("error %v was not classified transient", err)
		}
	}
	if isTransientFeedReadError(&os.PathError{Op: "open", Path: "current.json", Err: windows.ERROR_ACCESS_DENIED}) {
		t.Fatal("access denied was classified transient")
	}
}
