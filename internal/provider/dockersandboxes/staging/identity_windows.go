//go:build windows

package staging

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func platformDirectoryIdentity(path string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(pointer, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return "", err
	}
	return fmt.Sprintf("windows:%08x:%08x%08x", information.VolumeSerialNumber, information.FileIndexHigh, information.FileIndexLow), nil
}
