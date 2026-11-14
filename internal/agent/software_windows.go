//go:build windows

package agent

import (
	"context"

	"golang.org/x/sys/windows/registry"

	"github.com/sxi9/msp/internal/protocol"
)

// registryLister reads installed software on Windows (SI1) from the uninstall
// registry keys: the 64-bit and 32-bit (WOW6432Node) machine views and the
// per-user view. Each subkey's DisplayName/DisplayVersion/Publisher and the
// hide/system/update markers are decoded into rawUninstallEntry and handed to the
// shared parseRegistry. This path cross-builds and its parser is unit-tested
// against a captured dump, but it is NOT VERIFIED on a real Windows host
// (docs/STATUS.md). The commandExecutor is unused here (the registry is read
// directly) and kept only to satisfy the shared constructor seam.
type registryLister struct{}

// newPlatformSoftwareLister builds the Windows (registry) software lister.
func newPlatformSoftwareLister(commandExecutor) softwareLister { return registryLister{} }

// uninstallRoots are the registry locations Programs-and-Features reads. The
// 64-bit view (WOW64_64KEY) and the 32-bit view (WOW64_32KEY of WOW6432Node)
// cover native and 32-bit installs; HKCU covers per-user installs.
var uninstallRoots = []struct {
	root  registry.Key
	path  string
	flags uint32
}{
	{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, registry.WOW64_64KEY},
	{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`, registry.WOW64_32KEY},
	{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`, 0},
}

// List enumerates every uninstall subkey across the roots, de-duplicates on
// name+version (a program can appear in more than one view), and returns the
// parsed inventory. Registry errors leave that root empty rather than failing.
func (registryLister) List(ctx context.Context) ([]protocol.SoftwareItem, error) {
	var raw []rawUninstallEntry
	seen := map[string]bool{}
	for _, r := range uninstallRoots {
		for _, e := range readUninstallKey(r.root, r.path, r.flags) {
			key := e.DisplayName + "\x00" + e.DisplayVersion
			if seen[key] {
				continue
			}
			seen[key] = true
			raw = append(raw, e)
		}
	}
	return parseRegistry(raw), nil
}

// readUninstallKey enumerates one uninstall key's subkeys and decodes each into a
// rawUninstallEntry. An unopenable key or subkey is skipped.
func readUninstallKey(root registry.Key, path string, flags uint32) []rawUninstallEntry {
	k, err := registry.OpenKey(root, path, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE|flags)
	if err != nil {
		return nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil
	}
	var out []rawUninstallEntry
	for _, name := range names {
		sk, err := registry.OpenKey(root, path+`\`+name, registry.QUERY_VALUE|flags)
		if err != nil {
			continue
		}
		e := rawUninstallEntry{
			DisplayName:      regValueString(sk, "DisplayName"),
			DisplayVersion:   regValueString(sk, "DisplayVersion"),
			Publisher:        regValueString(sk, "Publisher"),
			ReleaseType:      regValueString(sk, "ReleaseType"),
			ParentKeyName:    regValueString(sk, "ParentKeyName"),
			SystemComponent:  int(regValueDword(sk, "SystemComponent")),
			WindowsInstaller: int(regValueDword(sk, "WindowsInstaller")),
		}
		sk.Close()
		out = append(out, e)
	}
	return out
}

// regValueString reads a REG_SZ value; "" on any error.
func regValueString(k registry.Key, name string) string {
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

// regValueDword reads a REG_DWORD value; 0 on any error.
func regValueDword(k registry.Key, name string) uint64 {
	v, _, err := k.GetIntegerValue(name)
	if err != nil {
		return 0
	}
	return v
}
