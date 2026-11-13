//go:build !linux && !windows

package agent

import (
	"crypto/sha256"
	"encoding/hex"
)

// machineID derives the device_uid from the hostname on platforms without a
// real machine id source (cross-build stubs only; see metrics_other.go).
func machineID() (string, error) {
	sum := sha256.Sum256([]byte(hostname()))
	return hex.EncodeToString(sum[:]), nil
}
