//go:build linux

package agent

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sxi9/msp/internal/protocol"
)

// Paths read by the Linux collector; variables so tests can point them at
// fixtures.
var (
	procStat    = "/proc/stat"
	procMeminfo = "/proc/meminfo"
	procUptime  = "/proc/uptime"
	procMounts  = "/proc/mounts"
	procCPUInfo = "/proc/cpuinfo"
	osRelease   = "/etc/os-release"
	powerSupply = "/sys/class/power_supply"
	dmiDir      = "/sys/class/dmi/id"
)

// diskFSTypes are the filesystem types reported as disks.
var diskFSTypes = map[string]bool{
	"ext4": true, "ext3": true, "ext2": true, "xfs": true, "btrfs": true,
	"zfs": true, "vfat": true, "ntfs": true, "f2fs": true,
}

// Metrics samples CPU, memory, disks, battery and IPs. Partial failures leave
// the affected field zero/absent; the error is only returned when nothing at
// all could be read.
func (c *collector) Metrics(ctx context.Context) (protocol.Metrics, error) {
	m := protocol.Metrics{Disks: []protocol.Disk{}, IPs: localIPs()}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if idle, total, err := readProcStat(procStat); err == nil {
		m.CPUPct = c.delta(idle, total)
	} else {
		note(err)
	}

	if total, avail, err := readMeminfo(procMeminfo); err == nil {
		m.MemTotalMB = int64(total / 1024)
		if avail > total {
			avail = total
		}
		m.MemUsedMB = int64((total - avail) / 1024)
	} else {
		note(err)
	}

	m.Disks = linuxDisks()

	if pct, charging, ok := readBattery(powerSupply); ok {
		m.BatteryPct = &pct
		m.BatteryCharging = &charging
	}

	if m.MemTotalMB == 0 && m.CPUPct == nil && len(m.Disks) == 0 && firstErr != nil {
		return m, firstErr
	}
	return m, nil
}

// Facts collects the slow-changing inventory. Errors are ignored and leave
// empty strings; the IPs slice is never nil.
func (c *collector) Facts() protocol.Facts {
	f := protocol.Facts{CPUCount: runtime.NumCPU(), IPs: localIPs()}
	var u unix.Utsname
	if err := unix.Uname(&u); err == nil {
		f.Kernel = unixString(u.Release[:])
	}
	f.CPUModel = cpuModel(procCPUInfo)
	if total, _, err := readMeminfo(procMeminfo); err == nil {
		f.MemTotalMB = int64(total / 1024)
	}
	f.Manufacturer = readSysString(filepath.Join(dmiDir, "sys_vendor"))
	f.Model = readSysString(filepath.Join(dmiDir, "product_name"))
	f.Serial = readSysString(filepath.Join(dmiDir, "product_serial"))
	f.RustdeskID = rustdeskID(context.Background())
	return f
}

// Uptime is seconds since boot from /proc/uptime (0 when unreadable).
func (c *collector) Uptime() int64 {
	b, err := os.ReadFile(procUptime)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// osVersion is PRETTY_NAME from /etc/os-release, falling back to "Linux".
func osVersion() string {
	b, err := os.ReadFile(osRelease)
	if err != nil {
		return "Linux"
	}
	if v := parseOSRelease(string(b)); v != "" {
		return v
	}
	return "Linux"
}

// parseOSRelease extracts PRETTY_NAME (quotes stripped) from os-release text.
func parseOSRelease(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "PRETTY_NAME=") {
			continue
		}
		v := strings.TrimPrefix(line, "PRETTY_NAME=")
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}

// readProcStat returns the idle (idle+iowait) and total jiffies from the
// aggregate "cpu" line.
func readProcStat(path string) (idle, total uint64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	return parseProcStat(string(b))
}

func parseProcStat(s string) (idle, total uint64, err error) {
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		for i, f := range fields[1:] {
			v, perr := strconv.ParseUint(f, 10, 64)
			if perr != nil {
				return 0, 0, perr
			}
			total += v
			// fields: user nice system idle iowait irq softirq steal guest guest_nice
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return idle, total, nil
	}
	return 0, 0, errors.New("no cpu line in /proc/stat")
}

// readMeminfo returns MemTotal and MemAvailable in kB.
func readMeminfo(path string) (total, avail uint64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	return parseMeminfo(string(b))
}

func parseMeminfo(s string) (total, avail uint64, err error) {
	var free uint64
	haveAvail := false
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
			haveAvail = true
		case "MemFree:":
			free = v
		}
	}
	if total == 0 {
		return 0, 0, errors.New("no MemTotal in /proc/meminfo")
	}
	if !haveAvail {
		avail = free
	}
	return total, avail, nil
}

// linuxDisks parses the mount table, keeps real block-device filesystems,
// dedupes by device (bind mounts, btrfs subvolumes) and statfs's each one.
func linuxDisks() []protocol.Disk {
	out := []protocol.Disk{}
	f, err := os.Open(procMounts)
	if err != nil {
		return out
	}
	defer f.Close()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		dev, mount, fstype := fields[0], unescapeMount(fields[1]), fields[2]
		if !strings.HasPrefix(dev, "/dev/") || !diskFSTypes[fstype] || seen[dev] {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(mount, &st); err != nil {
			continue
		}
		seen[dev] = true
		bs := uint64(st.Frsize)
		if bs == 0 {
			bs = uint64(st.Bsize)
		}
		total := st.Blocks * bs
		used := (st.Blocks - st.Bfree) * bs
		totalGB := bytesToGB(total)
		if totalGB == 0 {
			// Below 0.05 GiB (boot stubs, tiny squashfs images): not a disk
			// worth alerting on, and a 0.0 total would divide by zero upstream.
			continue
		}
		out = append(out, protocol.Disk{Mount: mount, TotalGB: totalGB, UsedGB: bytesToGB(used)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces etc.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// readBattery reads the first BAT* supply: capacity and whether it charges.
func readBattery(dir string) (pct int, charging bool, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false, false
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "BAT") {
			continue
		}
		cap := readSysString(filepath.Join(dir, e.Name(), "capacity"))
		v, err := strconv.Atoi(cap)
		if err != nil {
			continue
		}
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		status := readSysString(filepath.Join(dir, e.Name(), "status"))
		return v, status == "Charging" || status == "Full", true
	}
	return 0, false, false
}

// cpuModel returns the first "model name" (or "Hardware"/"Model" on ARM
// boards) from /proc/cpuinfo.
func cpuModel(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fallback := ""
	for _, line := range strings.Split(string(b), "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name":
			if v != "" {
				return v
			}
		case "Hardware", "Model":
			if fallback == "" {
				fallback = v
			}
		}
	}
	return fallback
}

// readSysString reads a one-line sysfs file, trimmed; "" on any error.
func readSysString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// unixString converts a NUL-terminated utsname field.
func unixString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
