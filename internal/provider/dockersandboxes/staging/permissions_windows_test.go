//go:build windows

package staging

import (
	"fmt"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRejectsUnexpectedWindowsDACLTrustee(t *testing.T) {
	staging, err := Open(filepath.Join(t.TempDir(), "staging"))
	if err != nil {
		t.Fatal(err)
	}
	owned, err := staging.CreateOwned("weak")
	if err != nil {
		t.Fatal(err)
	}
	path := owned.Path
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;GR;;;WD)", user.User.Sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := staging.VerifyOwnedEmpty("weak", owned.Identity); err == nil {
		t.Fatal("staging directory with an unexpected Everyone ACE was accepted")
	}
}
