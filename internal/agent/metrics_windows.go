//go:build windows

package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/sxi9/msp/internal/protocol"
)

// kernel32 procedures that golang.org/x/sys/windows does not wrap. They are
// resolved lazily on first use; a missing export surfaces as a Call error.
var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (sysinfoapi.h). Length must be set to
// the struct size before the call.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// systemPowerStatus mirrors SYSTEM_POWER_STATUS (winbase.h).
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

const (
	batteryFlagNoBattery = 128 // SYSTEM_POWER_STATUS.BatteryFlag: no system battery
	batteryPctUnknown    = 255 // SYSTEM_POWER_STATUS.BatteryLifePercent: unknown
	acLineOnline         = 1   // SYSTEM_POWER_STATUS.ACLineStatus: mains power
)

// Registry paths for inventory strings (always the 64-bit view).
const (
	regCurrentVersion = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	regCPU0           = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	regBIOS           = `HARDWARE\DESCRIPTION\System\BIOS`
)

// Metrics samples CPU, memory, fixed disks, battery and IPs. Partial failures
// leave the affected field zero/absent; the error is only returned when
// neither CPU, memory nor disks could be read.
func (c *collector) Metrics(ctx context.Context) (protocol.Metrics, error) {
	m := protocol.Metrics{Disks: []protocol.Disk{}, IPs: localIPs()}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if idle, total, err := systemTimes(); err == nil {
		m.CPUPct = c.delta(idle, total)
	} else {
		note(err)
	}

	if ms, err := memoryStatus(); err == nil {
		m.MemTotalMB = int64(ms.TotalPhys / (1 << 20))
		avail := ms.AvailPhys
		if avail > ms.TotalPhys {
			avail = ms.TotalPhys
		}
		m.MemUsedMB = int64((ms.TotalPhys - avail) / (1 << 20))
	} else {
		note(err)
	}

	m.Disks = windowsDisks()

	if pct, charging, ok := powerStatus(); ok {
		m.BatteryPct = &pct
		m.BatteryCharging = &charging
	}

	if m.MemTotalMB == 0 && m.CPUPct == nil && len(m.Disks) == 0 && firstErr != nil {
		return m, firstErr
	}
	return m, nil
}

// Facts collects the slow-changing inventory. Registry errors are ignored and
// leave empty strings; the IPs slice is never nil. Serial is empty on Windows
// (reading it needs WMI, which is out of scope for a dependency-free agent).
func (c *collector) Facts() protocol.Facts {
	f := protocol.Facts{CPUCount: runtime.NumCPU(), IPs: localIPs()}
	v := windows.RtlGetVersion()
	f.Kernel = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	f.CPUModel = regString(regCPU0, "ProcessorNameString")
	f.Manufacturer = regString(regBIOS, "SystemManufacturer")
	f.Model = regString(regBIOS, "SystemProductName")
	if ms, err := memoryStatus(); err == nil {
		f.MemTotalMB = int64(ms.TotalPhys / (1 << 20))
	}
	f.RustdeskID = rustdeskID(context.Background())
	return f
}

// Uptime is seconds since boot from GetTickCount64 (0 when unavailable).
func (c *collector) Uptime() int64 {
	r1, r2, _ := procGetTickCount64.Call()
	ms := uint64(r1)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		// On 32-bit Windows the 64-bit result is split across EAX:EDX.
		ms = uint64(uint32(r1)) | uint64(uint32(r2))<<32
	}
	return int64(ms / 1000)
}

// osVersion is "<ProductName> (Windows <Major>.<Minor> build <Build>)", or
// just the version part when the registry cannot be read. ProductName still
// says "Windows 10 ..." on Windows 11, so builds >= 22000 are relabelled.
func osVersion() string {
	v := windows.RtlGetVersion()
	base := fmt.Sprintf("Windows %d.%d build %d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	name := regString(regCurrentVersion, "ProductName")
	if name == "" {
		return base
	}
	if v.MajorVersion == 10 && v.BuildNumber >= 22000 && len(name) >= 10 && name[:10] == "Windows 10" {
		name = "Windows 11" + name[10:]
	}
	return name + " (" + base + ")"
}

// systemTimes returns the idle and total (kernel+user; kernel includes idle)
// 100 ns tick counters from GetSystemTimes.
func systemTimes() (idle, total uint64, err error) {
	var idleFT, kernelFT, userFT windows.Filetime
	r1, _, e := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFT)),
		uintptr(unsafe.Pointer(&kernelFT)),
		uintptr(unsafe.Pointer(&userFT)),
	)
	if r1 == 0 {
		return 0, 0, fmt.Errorf("GetSystemTimes: %w", callError(e))
	}
	idle = filetimeToUint64(idleFT)
	total = filetimeToUint64(kernelFT) + filetimeToUint64(userFT)
	return idle, total, nil
}

// memoryStatus calls GlobalMemoryStatusEx.
func memoryStatus() (memoryStatusEx, error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r1, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r1 == 0 {
		return ms, fmt.Errorf("GlobalMemoryStatusEx: %w", callError(e))
	}
	if ms.TotalPhys == 0 {
		return ms, errors.New("GlobalMemoryStatusEx: zero physical memory")
	}
	return ms, nil
}

// windowsDisks lists every fixed drive (DRIVE_FIXED) with its size and usage
// in GiB, mount "C:" style, sorted by letter.
func windowsDisks() []protocol.Disk {
	out := []protocol.Disk{}
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return out
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		root, err := windows.UTF16PtrFromString(letter + `:\`)
		if err != nil {
			continue
		}
		if windows.GetDriveType(root) != windows.DRIVE_FIXED {
			continue
		}
		var freeToCaller, total, totalFree uint64
		if err := windows.GetDiskFreeSpaceEx(root, &freeToCaller, &total, &totalFree); err != nil {
			continue
		}
		totalGB := bytesToGB(total)
		if totalGB == 0 {
			continue
		}
		used := total
		if totalFree <= total {
			used = total - totalFree
		}
		out = append(out, protocol.Disk{Mount: letter + ":", TotalGB: totalGB, UsedGB: bytesToGB(used)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// powerStatus reads the battery through GetSystemPowerStatus. ok is false
// when there is no battery or its level is unknown (255).
func powerStatus() (pct int, charging bool, ok bool) {
	var ps systemPowerStatus
	r1, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&ps)))
	if r1 == 0 {
		return 0, false, false
	}
	if ps.BatteryFlag&batteryFlagNoBattery != 0 || ps.BatteryLifePercent == batteryPctUnknown {
		return 0, false, false
	}
	pct = int(ps.BatteryLifePercent)
	if pct > 100 {
		pct = 100
	}
	return pct, ps.ACLineStatus == acLineOnline, true
}

// regString reads a REG_SZ value from HKLM in the 64-bit registry view; ""
// on any error.
func regString(path, name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return trimNUL(v)
}

// trimNUL strips trailing spaces and NULs some firmware strings carry.
func trimNUL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == 0 || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return s
}

// filetimeToUint64 joins the two halves of a FILETIME.
func filetimeToUint64(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// callError turns the errno from LazyProc.Call into an error, treating
// ERROR_SUCCESS as an unknown failure (Call always returns a non-nil error).
func callError(e error) error {
	if errno, ok := e.(windows.Errno); ok && errno == 0 {
		return errors.New("call failed")
	}
	return e
}
