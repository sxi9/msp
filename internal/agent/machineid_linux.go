//go:build linux

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"os"
	"strings"
)

// machineIDFiles are tried in order; variables so tests can redirect them.
var machineIDFiles = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

// machineID returns the stable device_uid for this host: /etc/machine-id,
// then /var/lib/dbus/machine-id, then sha256(hostname + first MAC) with a
// logged warning because that fallback changes when the NIC does.
func machineID() (string, error) {
	for _, p := range machineIDFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	mac := firstMAC()
	host := hostname()
	if host == "unknown" && mac == "" {
		return "", errors.New("machine id: no machine-id file, hostname or MAC address available")
	}
	slog.Warn("no /etc/machine-id; deriving device id from hostname and MAC address", "hostname", host, "mac", mac)
	sum := sha256.Sum256([]byte(host + "|" + mac))
	return hex.EncodeToString(sum[:]), nil
}

// firstMAC returns the hardware address of the first non-loopback interface
// that has one, in interface index order.
func firstMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || len(ifc.HardwareAddr) == 0 {
			continue
		}
		return ifc.HardwareAddr.String()
	}
	return ""
}
