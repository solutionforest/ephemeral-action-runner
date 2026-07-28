//go:build windows

package staging

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func restrictPlatformPermissions(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current process user: %w", err)
	}
	sddl := fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)", user.User.Sid.String())
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("build private staging security descriptor: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read private staging DACL: %w", err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("set private staging DACL: %w", err)
	}
	return nil
}

func validatePlatformPermissions(path string, _ os.FileInfo) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read staging DACL: %w", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read staging DACL control: %w", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("DACL inherits access from a parent")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current process user: %w", err)
	}
	sddl := descriptor.String()
	currentSID := user.User.Sid.String()
	remaining := sddl
	seenCurrent := false
	seenSystem := false
	aceCount := 0
	for {
		start := strings.IndexByte(remaining, '(')
		if start < 0 {
			break
		}
		remaining = remaining[start+1:]
		end := strings.IndexByte(remaining, ')')
		if end < 0 {
			return fmt.Errorf("DACL contains malformed SDDL")
		}
		fields := strings.Split(remaining[:end], ";")
		remaining = remaining[end+1:]
		if len(fields) != 6 || fields[0] != "A" || fields[2] != "FA" {
			return fmt.Errorf("DACL contains an unexpected access rule")
		}
		aceCount++
		switch fields[5] {
		case currentSID:
			if seenCurrent {
				return fmt.Errorf("DACL contains duplicate current-user access")
			}
			seenCurrent = true
		case "SY", "S-1-5-18":
			if seenSystem {
				return fmt.Errorf("DACL contains duplicate SYSTEM access")
			}
			seenSystem = true
		default:
			return fmt.Errorf("DACL grants an unexpected trustee")
		}
	}
	if aceCount != 2 || !seenCurrent || !seenSystem {
		return fmt.Errorf("DACL must grant full access only to the current process identity and SYSTEM")
	}
	return nil
}
