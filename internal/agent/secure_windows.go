//go:build windows

package agent

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictToPrivileged replaces the DACL of path with one that grants full
// control to SYSTEM and BUILTIN\Administrators only and disables
// inheritance, so the file (or directory and, through inheritance, what is
// created inside it) is no longer readable by BUILTIN\Users, which the
// default ACL of %ProgramData% grants. Go's permission bits are a no-op on
// Windows (0600 only toggles the read-only attribute), so this is what
// makes the "0600" of agent.json and rustdesk-password true there. Needs an
// elevated caller; the error is returned, never ignored, because a config
// directory readable by every local user leaks the device token and the
// permanent RustDesk password.
func restrictToPrivileged(path string, dir bool) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("restrict %s: SYSTEM sid: %w", path, err)
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("restrict %s: Administrators sid: %w", path, err)
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(system),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(admins),
			},
		},
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("restrict %s: build acl: %w", path, err)
	}
	// PROTECTED_DACL_SECURITY_INFORMATION is what icacls /inheritance:r does:
	// the ACEs inherited from the parent (ProgramData's BUILTIN\Users:RX) are
	// dropped instead of merged.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return fmt.Errorf("restrict %s to SYSTEM and Administrators: %w (run as Administrator)", path, err)
	}
	return nil
}
