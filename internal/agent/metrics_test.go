//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCpuPct(t *testing.T) {
	cases := []struct {
		idle, total uint64
		want        *float64
	}{
		{0, 0, nil},
		{100, 100, f(0)},
		{0, 100, f(100)},
		{50, 100, f(50)},
		{875, 1000, f(12.5)},
		{877, 1000, f(12.3)},
		{1, 3, f(66.7)},
		{200, 100, f(0)}, // idle > total is clamped, never negative
	}
	for _, c := range cases {
		got := cpuPct(c.idle, c.total)
		switch {
		case got == nil && c.want == nil:
		case got == nil || c.want == nil:
			t.Errorf("cpuPct(%d,%d) = %v, want %v", c.idle, c.total, got, c.want)
		case *got != *c.want:
			t.Errorf("cpuPct(%d,%d) = %v, want %v", c.idle, c.total, *got, *c.want)
		}
	}
}

func f(v float64) *float64 { return &v }

func TestPctRound(t *testing.T) {
	cases := map[float64]float64{12.34: 12.3, 12.35: 12.4, 0: 0, 99.96: 100, -1.26: -1.3}
	for in, want := range cases {
		if got := pctRound(in); got != want {
			t.Errorf("pctRound(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestCollectorDelta(t *testing.T) {
	c := newCollector()
	if got := c.delta(100, 1000); got != nil {
		t.Fatalf("first sample must be nil, got %v", *got)
	}
	got := c.delta(150, 1100) // 50 idle of 100 total -> 50%
	if got == nil || *got != 50 {
		t.Fatalf("second sample = %v, want 50", got)
	}
	if got := c.delta(150, 1100); got != nil {
		t.Fatalf("no progress must give nil, got %v", *got)
	}
	if got := c.delta(10, 20); got != nil {
		t.Fatalf("counters going backwards must give nil, got %v", *got)
	}
}

func TestParseProcStat(t *testing.T) {
	s := "cpu  61542 0 6198 1698799 1564 0 1002 2379 0 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n"
	idle, total, err := parseProcStat(s)
	if err != nil {
		t.Fatal(err)
	}
	if idle != 1698799+1564 {
		t.Fatalf("idle = %d", idle)
	}
	if total != 61542+6198+1698799+1564+1002+2379 {
		t.Fatalf("total = %d", total)
	}
	if _, _, err := parseProcStat("intr 1 2 3\n"); err == nil {
		t.Fatal("expected error without cpu line")
	}
}

func TestParseMeminfo(t *testing.T) {
	total, avail, err := parseMeminfo("MemTotal:       16472076 kB\nMemFree:  100 kB\nMemAvailable:   15786788 kB\n")
	if err != nil || total != 16472076 || avail != 15786788 {
		t.Fatalf("got %d %d %v", total, avail, err)
	}
	total, avail, err = parseMeminfo("MemTotal: 1000 kB\nMemFree: 400 kB\n")
	if err != nil || total != 1000 || avail != 400 {
		t.Fatalf("fallback to MemFree: got %d %d %v", total, avail, err)
	}
	if _, _, err := parseMeminfo("Nothing: 1 kB\n"); err == nil {
		t.Fatal("expected error without MemTotal")
	}
}

func TestParseOSRelease(t *testing.T) {
	if got := parseOSRelease("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.5 LTS\"\n"); got != "Ubuntu 24.04.5 LTS" {
		t.Fatalf("got %q", got)
	}
	if got := parseOSRelease("NAME=x\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/my\040disk`); got != "/mnt/my disk" {
		t.Fatalf("got %q", got)
	}
	if got := unescapeMount("/plain"); got != "/plain" {
		t.Fatalf("got %q", got)
	}
}

func TestReadBattery(t *testing.T) {
	dir := t.TempDir()
	if _, _, ok := readBattery(dir); ok {
		t.Fatal("no BAT dir must report absent")
	}
	bat := filepath.Join(dir, "BAT0")
	os.MkdirAll(bat, 0o755)
	os.WriteFile(filepath.Join(bat, "capacity"), []byte("87\n"), 0o644)
	os.WriteFile(filepath.Join(bat, "status"), []byte("Discharging\n"), 0o644)
	pct, charging, ok := readBattery(dir)
	if !ok || pct != 87 || charging {
		t.Fatalf("got %d %v %v", pct, charging, ok)
	}
	os.WriteFile(filepath.Join(bat, "status"), []byte("Full\n"), 0o644)
	if _, charging, _ := readBattery(dir); !charging {
		t.Fatal("Full must count as charging")
	}
}

func TestCpuModel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cpuinfo")
	os.WriteFile(p, []byte("processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Core(TM) i5\n"), 0o644)
	if got := cpuModel(p); got != "Intel(R) Core(TM) i5" {
		t.Fatalf("got %q", got)
	}
	os.WriteFile(p, []byte("processor\t: 0\nHardware\t: BCM2835\n"), 0o644)
	if got := cpuModel(p); got != "BCM2835" {
		t.Fatalf("got %q", got)
	}
}

func TestMetricsOnThisHost(t *testing.T) {
	c := newCollector()
	m, err := c.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotalMB <= 0 || m.MemUsedMB < 0 || m.MemUsedMB > m.MemTotalMB {
		t.Fatalf("memory = %d/%d MB", m.MemUsedMB, m.MemTotalMB)
	}
	if len(m.Disks) < 1 {
		t.Fatalf("expected at least one disk, got %+v", m.Disks)
	}
	for _, d := range m.Disks {
		if d.Mount == "" || d.TotalGB <= 0 || d.UsedGB < 0 || d.UsedGB > d.TotalGB {
			t.Fatalf("bad disk %+v", d)
		}
	}
	if m.CPUPct != nil {
		t.Fatalf("first sample must be null, got %v", *m.CPUPct)
	}
	if m.IPs == nil {
		t.Fatal("ips must be [] not null")
	}
	m2, err := c.Metrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The second sample has a percentage when any jiffy ticked in between;
	// on an idle box it may still be nil, so only check the range.
	if m2.CPUPct != nil && (*m2.CPUPct < 0 || *m2.CPUPct > 100) {
		t.Fatalf("cpu_pct out of range: %v", *m2.CPUPct)
	}
	if c.Uptime() <= 0 {
		t.Fatal("uptime must be positive")
	}
	if osVersion() == "" {
		t.Fatal("osVersion empty")
	}
	facts := c.Facts()
	if facts.Kernel == "" || facts.CPUCount < 1 || facts.MemTotalMB <= 0 || facts.IPs == nil {
		t.Fatalf("facts = %+v", facts)
	}
	id, err := machineID()
	if err != nil || id == "" {
		t.Fatalf("machineID = %q, %v", id, err)
	}
	id2, _ := machineID()
	if id != id2 {
		t.Fatal("machineID must be stable")
	}
}

func TestMachineIDFallback(t *testing.T) {
	old := machineIDFiles
	machineIDFiles = []string{filepath.Join(t.TempDir(), "missing")}
	defer func() { machineIDFiles = old }()
	id, err := machineID()
	if err != nil || len(id) != 64 {
		t.Fatalf("fallback id = %q, %v", id, err)
	}
}

func TestLocalIPsAndHelpers(t *testing.T) {
	ips := localIPs()
	if ips == nil {
		t.Fatal("localIPs must not return nil")
	}
	for _, ip := range ips {
		if ip == "127.0.0.1" || ip == "::1" {
			t.Fatalf("loopback leaked: %v", ips)
		}
	}
	if arch() == "" || hostname() == "" {
		t.Fatal("arch/hostname empty")
	}
	if got := bytesToGB(512 * 1024 * 1024); got != 0.5 {
		t.Fatalf("bytesToGB = %v", got)
	}
}
