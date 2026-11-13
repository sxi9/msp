//go:build windows

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// machineGUIDKey is the registry key that holds the per-installation
// MachineGuid (created by the Cryptographic Services at setup).
const machineGUIDKey = `SOFTWARE\Microsoft\Cryptography`

// machineID returns the stable device_uid for this host: MachineGuid from
// HKLM\SOFTWARE\Microsoft\Cryptography (64-bit view, so a 32-bit build sees
// the same value), falling back to sha256(hostname) with a logged warning.
func machineID() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, machineGUIDKey, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err == nil {
		defer k.Close()
		if v, _, err := k.GetStringValue("MachineGuid"); err == nil {
			if id := strings.TrimSpace(v); id != "" {
				return strings.ToLower(id), nil
			}
		}
	}
	host := hostname()
	slog.Warn("MachineGuid not readable; deriving device id from hostname", "hostname", host)
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:]), nil
}
