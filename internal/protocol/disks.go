package protocol

import (
	"math"
	"strings"
)

// MinAlertDiskGB is the smallest filesystem that counts towards a device's
// disk_used_pct. Separate /boot and EFI partitions are routinely 0.5-1 GiB
// and sit at 90 %+ on a healthy host, so they must not drive the Fleet Disk
// column or the disk_full alert.
const MinAlertDiskGB = 2.0

// DiskCounts reports whether a mounted filesystem is taken into account for
// disk_used_pct: it must be at least MinAlertDiskGB and not a boot or EFI
// partition.
func DiskCounts(d Disk) bool {
	if d.TotalGB < MinAlertDiskGB {
		return false
	}
	m := strings.ToLower(d.Mount)
	switch {
	case m == "/boot" || strings.HasPrefix(m, "/boot/"):
		return false
	case m == "/efi" || strings.HasPrefix(m, "/efi/"):
		return false
	}
	return true
}

// DiskUsedPct derives the device-level disk usage from a metrics sample: the
// used percentage (one decimal) of the fullest filesystem that DiskCounts
// accepts, with its mount. When no filesystem qualifies (a very small root,
// a lab VM) the fullest of all reported filesystems is used instead, so a
// device with any disk at all always has a value. ok is false when there is
// no disk with a positive total.
func DiskUsedPct(disks []Disk) (pct float64, mount string, ok bool) {
	best := func(filter func(Disk) bool) (float64, string, bool) {
		var (
			top      float64
			topMount string
			found    bool
		)
		for _, d := range disks {
			if d.TotalGB <= 0 || !filter(d) {
				continue
			}
			v := math.Round(d.UsedGB*1000/d.TotalGB) / 10
			if !found || v > top {
				top, topMount, found = v, d.Mount, true
			}
		}
		return top, topMount, found
	}
	if pct, mount, ok = best(DiskCounts); ok {
		return pct, mount, true
	}
	return best(func(Disk) bool { return true })
}
