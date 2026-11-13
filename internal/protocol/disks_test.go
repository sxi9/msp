package protocol

import "testing"

func TestDiskUsedPctIgnoresSmallAndBootMounts(t *testing.T) {
	disks := []Disk{
		{Mount: "/", TotalGB: 476, UsedGB: 201.3},
		{Mount: "/boot", TotalGB: 1, UsedGB: 0.95},
		{Mount: "/boot/efi", TotalGB: 0.5, UsedGB: 0.1},
		{Mount: "/opt/tiny", TotalGB: 0.4, UsedGB: 0.3},
		{Mount: "/data", TotalGB: 100, UsedGB: 90},
	}
	pct, mount, ok := DiskUsedPct(disks)
	if !ok || pct != 90 || mount != "/data" {
		t.Fatalf("DiskUsedPct = %v %q %v, want 90 /data true", pct, mount, ok)
	}

	// A large /boot still does not count; EFI on its own mount neither.
	if DiskCounts(Disk{Mount: "/boot", TotalGB: 4, UsedGB: 3}) || DiskCounts(Disk{Mount: "/efi", TotalGB: 4}) {
		t.Fatal("boot mounts must not count")
	}
	if !DiskCounts(Disk{Mount: "C:", TotalGB: 237, UsedGB: 100}) || !DiskCounts(Disk{Mount: "/data", TotalGB: 2, UsedGB: 1}) {
		t.Fatal("ordinary volumes must count")
	}
}

func TestDiskUsedPctFallsBackToAnyDisk(t *testing.T) {
	// Only tiny filesystems: the fullest of them is still reported.
	pct, mount, ok := DiskUsedPct([]Disk{{Mount: "/", TotalGB: 1, UsedGB: 0.5}, {Mount: "/boot", TotalGB: 1, UsedGB: 0.9}})
	if !ok || pct != 90 || mount != "/boot" {
		t.Fatalf("fallback = %v %q %v", pct, mount, ok)
	}
	if _, _, ok := DiskUsedPct(nil); ok {
		t.Fatal("no disks must not be ok")
	}
	if _, _, ok := DiskUsedPct([]Disk{{Mount: "/", TotalGB: 0, UsedGB: 0}}); ok {
		t.Fatal("zero total must not be ok")
	}
	// Rounding to one decimal.
	if pct, _, _ := DiskUsedPct([]Disk{{Mount: "/", TotalGB: 16000, UsedGB: 8120}}); pct != 50.8 {
		t.Fatalf("rounding = %v", pct)
	}
}
