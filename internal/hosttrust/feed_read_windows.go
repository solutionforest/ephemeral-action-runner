//go:build windows

package hosttrust

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isPlatformTransientFeedReadError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
