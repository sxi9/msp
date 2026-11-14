//go:build linux

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// fixedNow returns a constant clock for deterministic reports.
func fixedNow() time.Time { return time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC) }

// recordedCall is one call the fakeExecutor saw.
type recordedCall struct {
	env  []string
	name string
	args []string
}

func (c recordedCall) argLine() string { return strings.Join(c.args, " ") }

func (c recordedCall) hasEnv(kv string) bool {
	for _, e := range c.env {
		if e == kv {
			return true
		}
	}
	return false
}

// fakeExecutor returns canned output and never starts a process, so no test
// touches the real package manager.
type fakeExecutor struct {
	calls   []recordedCall
	handler func(recordedCall) (cmdResult, error)
}

func (f *fakeExecutor) run(ctx context.Context, env []string, name string, args ...string) (cmdResult, error) {
	call := recordedCall{env: env, name: name, args: args}
	f.calls = append(f.calls, call)
	if f.handler != nil {
		return f.handler(call)
	}
	return cmdResult{}, nil
}

// isAptScan reports whether a recorded call is `apt-get -s dist-upgrade`.
func isAptScan(c recordedCall) bool {
	return c.name == "apt-get" && len(c.args) >= 2 && c.args[0] == "-s"
}

// fakeManager is an injectable patchManager for collector and RunPatch tests.
type fakeManager struct {
	report     protocol.PatchReport
	scans      int
	installs   []installCall
	installOut string
	rebooted   bool
	installErr error
}

type installCall struct {
	mode   string
	ids    []string
	reboot string
}

func (m *fakeManager) Scan(ctx context.Context) (protocol.PatchReport, error) {
	m.scans++
	return m.report, nil
}

func (m *fakeManager) Install(ctx context.Context, mode string, ids []string, reboot string) (string, bool, error) {
	m.installs = append(m.installs, installCall{mode: mode, ids: ids, reboot: reboot})
	return m.installOut, m.rebooted, m.installErr
}

func TestParseAptSimulate(t *testing.T) {
	data, err := os.ReadFile("testdata/apt-simulate.txt")
	if err != nil {
		t.Fatal(err)
	}
	ups := parseAptSimulate(string(data))
	if len(ups) != 5 {
		t.Fatalf("got %d updates, want 5: %+v", len(ups), ups)
	}
	byName := map[string]protocol.PatchUpdate{}
	for _, u := range ups {
		byName[u.Name] = u
	}
	ssl := byName["libssl3"]
	if ssl.Current != "3.0.2-0ubuntu1.18" || ssl.New != "3.0.2-0ubuntu1.19" || !ssl.Security {
		t.Fatalf("libssl3 parsed wrong: %+v", ssl)
	}
	if ssl.Source != "Ubuntu:24.04/noble-security" {
		t.Fatalf("libssl3 source = %q", ssl.Source)
	}
	vim := byName["vim-common"]
	if vim.Current != "2:9.1.0016-1ubuntu7" || vim.New != "2:9.1.0101-1ubuntu7.2" || vim.Security {
		t.Fatalf("vim-common parsed wrong (epoch/security): %+v", vim)
	}
	// A brand-new dependency has no [current] bracket.
	hdr := byName["linux-headers-6.8.0-48"]
	if hdr.Current != "" || hdr.New != "6.8.0-48.48" || !hdr.Security {
		t.Fatalf("new package parsed wrong: %+v", hdr)
	}
	sec := 0
	for _, u := range ups {
		if u.Security {
			sec++
		}
	}
	if sec != 3 {
		t.Fatalf("security count = %d, want 3", sec)
	}
}

func TestAptManagerScan(t *testing.T) {
	data, err := os.ReadFile("testdata/apt-simulate.txt")
	if err != nil {
		t.Fatal(err)
	}
	// A reboot-required file that exists.
	rr := filepath.Join(t.TempDir(), "reboot-required")
	if err := os.WriteFile(rr, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	old := rebootRequiredFile
	rebootRequiredFile = rr
	defer func() { rebootRequiredFile = old }()

	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if isAptScan(c) {
			return cmdResult{Stdout: string(data)}, nil
		}
		t.Fatalf("unexpected call %q %v", c.name, c.args)
		return cmdResult{}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	report, err := m.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Manager != protocol.PatchManagerApt || report.Error != "" {
		t.Fatalf("report = %+v", report)
	}
	if report.Count != 5 || report.SecurityCount != 3 {
		t.Fatalf("counts = %d/%d, want 5/3", report.Count, report.SecurityCount)
	}
	if !report.RebootRequired {
		t.Fatal("reboot_required must be true when the file exists")
	}
	if !report.CheckedAt.Equal(fixedNow()) {
		t.Fatalf("checked_at = %v", report.CheckedAt)
	}
	// Security updates sort first.
	if !report.Updates[0].Security || report.Updates[len(report.Updates)-1].Security {
		t.Fatalf("updates not sorted security-first: %+v", report.Updates)
	}
}

func TestAptManagerScanLockHeld(t *testing.T) {
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{ExitCode: 100, Stderr: "E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by another process"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	report, _ := m.Scan(context.Background())
	if report.Error == "" || !strings.Contains(report.Error, "Could not get lock") {
		t.Fatalf("expected lock error, got %q", report.Error)
	}
	if len(report.Updates) != 0 {
		t.Fatalf("a failed scan must carry no updates, got %+v", report.Updates)
	}
}

func TestAptManagerInstallAll(t *testing.T) {
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{Stdout: "upgraded 4 packages"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	out, rebooted, err := m.Install(context.Background(), protocol.PatchModeAll, nil, protocol.PatchRebootNever)
	if err != nil || rebooted {
		t.Fatalf("install all: out=%q rebooted=%v err=%v", out, rebooted, err)
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(fe.calls))
	}
	c := fe.calls[0]
	if c.name != "apt-get" || c.argLine() != "-y upgrade" {
		t.Fatalf("argv = %q %v", c.name, c.args)
	}
	if !c.hasEnv("DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("missing noninteractive env: %v", c.env)
	}
}

func TestAptManagerInstallSecurity(t *testing.T) {
	data, err := os.ReadFile("testdata/apt-simulate.txt")
	if err != nil {
		t.Fatal(err)
	}
	var installArgs []string
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if isAptScan(c) {
			return cmdResult{Stdout: string(data)}, nil
		}
		installArgs = c.args
		return cmdResult{Stdout: "done"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	_, _, err = m.Install(context.Background(), protocol.PatchModeSecurity, nil, protocol.PatchRebootNever)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Join(installArgs, " ")
	if !strings.HasPrefix(line, "-y --only-upgrade install ") {
		t.Fatalf("security install argv = %q", line)
	}
	for _, name := range []string{"libssl3", "openssl", "linux-headers-6.8.0-48"} {
		if !strings.Contains(line, name) {
			t.Fatalf("security install missing %q: %q", name, line)
		}
	}
	if strings.Contains(line, "curl") || strings.Contains(line, "vim-common") {
		t.Fatalf("security install included a non-security package: %q", line)
	}
}

func TestAptManagerInstallSecurityNonePending(t *testing.T) {
	// A scan with no security updates: install must be a no-op, not apt-get with
	// an empty package list.
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if isAptScan(c) {
			return cmdResult{Stdout: "Inst curl [1] (2 Ubuntu:24.04/noble-updates [amd64])\n"}, nil
		}
		t.Fatalf("install must not run with no security updates: %v", c.args)
		return cmdResult{}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	out, rebooted, err := m.Install(context.Background(), protocol.PatchModeSecurity, nil, protocol.PatchRebootNever)
	if err != nil || rebooted || !strings.Contains(out, "no security updates") {
		t.Fatalf("out=%q rebooted=%v err=%v", out, rebooted, err)
	}
}

func TestAptManagerInstallIDs(t *testing.T) {
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{Stdout: "done"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	_, _, err := m.Install(context.Background(), protocol.PatchModeIDs, []string{"curl", "openssl"}, protocol.PatchRebootNever)
	if err != nil {
		t.Fatal(err)
	}
	if len(fe.calls) != 1 || fe.calls[0].argLine() != "-y --only-upgrade install curl openssl" {
		t.Fatalf("ids install argv = %v", fe.calls)
	}
}

func TestAptManagerInstallRebootIfRequired(t *testing.T) {
	rr := filepath.Join(t.TempDir(), "reboot-required")
	if err := os.WriteFile(rr, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	old := rebootRequiredFile
	rebootRequiredFile = rr
	defer func() { rebootRequiredFile = old }()

	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{Stdout: "ok"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	_, rebooted, err := m.Install(context.Background(), protocol.PatchModeAll, nil, protocol.PatchRebootIfRequired)
	if err != nil || !rebooted {
		t.Fatalf("expected reboot: rebooted=%v err=%v", rebooted, err)
	}
	var sawReboot bool
	for _, c := range fe.calls {
		if c.name == "systemctl" && c.argLine() == "reboot" {
			sawReboot = true
		}
	}
	if !sawReboot {
		t.Fatalf("systemctl reboot was not called: %v", fe.calls)
	}
}

func TestAptManagerInstallRebootNotNeeded(t *testing.T) {
	old := rebootRequiredFile
	rebootRequiredFile = filepath.Join(t.TempDir(), "absent")
	defer func() { rebootRequiredFile = old }()

	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if c.name == "systemctl" {
			t.Fatalf("must not reboot when no reboot is required")
		}
		return cmdResult{Stdout: "ok"}, nil
	}}
	m := &aptManager{exec: fe, now: fixedNow}
	_, rebooted, err := m.Install(context.Background(), protocol.PatchModeAll, nil, protocol.PatchRebootIfRequired)
	if err != nil || rebooted {
		t.Fatalf("rebooted=%v err=%v", rebooted, err)
	}
}

func TestParseWindowsUpdates(t *testing.T) {
	data, err := os.ReadFile("testdata/windows-update.json")
	if err != nil {
		t.Fatal(err)
	}
	report, err := parseWindowsUpdates(data, fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if report.Manager != protocol.PatchManagerWU || report.Error != "" {
		t.Fatalf("report = %+v", report)
	}
	if !report.RebootRequired {
		t.Fatal("reboot_required must be true")
	}
	if report.Count != 4 || report.SecurityCount != 2 {
		t.Fatalf("counts = %d/%d, want 4/2", report.Count, report.SecurityCount)
	}
	// Security first, and the source carries the KB and severity.
	if !report.Updates[0].Security {
		t.Fatalf("first update should be security: %+v", report.Updates[0])
	}
	if !strings.Contains(report.Updates[0].Source, "KB5044284") || !strings.Contains(report.Updates[0].Source, "Security Updates") {
		t.Fatalf("source missing KB/category: %q", report.Updates[0].Source)
	}
	if report.Updates[len(report.Updates)-1].Security {
		t.Fatalf("last update should not be security: %+v", report.Updates[len(report.Updates)-1])
	}
}

func TestParseWindowsInstallResult(t *testing.T) {
	rebooted, msg := parseWindowsInstallResult("install: X\nresult code: 2\n{\"rebooted\":true,\"error\":\"\"}")
	if !rebooted || msg != "" {
		t.Fatalf("got rebooted=%v msg=%q", rebooted, msg)
	}
	_, msg = parseWindowsInstallResult("{\"rebooted\":false,\"error\":\"0x80240022\"}")
	if msg != "0x80240022" {
		t.Fatalf("error not parsed: %q", msg)
	}
	rebooted, msg = parseWindowsInstallResult("just a log with no json\n")
	if rebooted || msg != "" {
		t.Fatalf("no-json fallback: rebooted=%v msg=%q", rebooted, msg)
	}
}

func TestFinalizePatchReport(t *testing.T) {
	r := protocol.PatchReport{
		Manager: protocol.PatchManagerApt,
		Updates: []protocol.PatchUpdate{
			{Name: "zlib", Security: false},
			{Name: "openssl", Security: true},
			{Name: "curl", Security: false},
			{Name: "bash", Security: true},
		},
	}
	got := finalizePatchReport(r, fixedNow())
	want := []string{"bash", "openssl", "curl", "zlib"} // security (by name), then the rest (by name)
	for i, name := range want {
		if got.Updates[i].Name != name {
			t.Fatalf("order[%d] = %q, want %q (%+v)", i, got.Updates[i].Name, name, got.Updates)
		}
	}
	if got.Count != 4 || got.SecurityCount != 2 {
		t.Fatalf("counts = %d/%d", got.Count, got.SecurityCount)
	}
	// Field length bounding.
	long := protocol.PatchReport{Manager: protocol.PatchManagerApt, Updates: []protocol.PatchUpdate{
		{Name: strings.Repeat("a", protocol.MaxPatchNameLen+50)},
	}}
	cutRep := finalizePatchReport(long, fixedNow())
	if len(cutRep.Updates[0].Name) != protocol.MaxPatchNameLen {
		t.Fatalf("name not bounded: %d", len(cutRep.Updates[0].Name))
	}
	// nil slice becomes [].
	empty := finalizePatchReport(protocol.PatchReport{Manager: protocol.PatchManagerNone}, fixedNow())
	if empty.Updates == nil {
		t.Fatal("updates must be [] not nil")
	}
}

func TestPatchCollectorReportCadence(t *testing.T) {
	m := &fakeManager{report: protocol.PatchReport{Manager: protocol.PatchManagerApt, Updates: []protocol.PatchUpdate{{Name: "curl"}}}}
	pc := newPatchCollectorWith(m)
	t0 := fixedNow()

	if r := pc.Report(context.Background(), t0); r == nil || m.scans != 1 {
		t.Fatalf("first report must scan: r=%v scans=%d", r, m.scans)
	}
	if r := pc.Report(context.Background(), t0.Add(time.Hour)); r != nil {
		t.Fatalf("within the interval Report must be nil, got %+v", r)
	}
	if m.scans != 1 {
		t.Fatalf("no extra scan within the interval, scans=%d", m.scans)
	}
	if r := pc.Report(context.Background(), t0.Add(patchInterval)); r == nil || m.scans != 2 {
		t.Fatalf("after the interval Report must scan again: r=%v scans=%d", r, m.scans)
	}
}

func TestClampPatchTimeout(t *testing.T) {
	cases := map[int]int{
		0:                              protocol.DefaultPatchTimeoutS,
		-5:                             1,
		60:                             60,
		protocol.MaxPatchTimeoutS:      protocol.MaxPatchTimeoutS,
		protocol.MaxPatchTimeoutS + 10: protocol.MaxPatchTimeoutS,
	}
	for in, want := range cases {
		if got := clampPatchTimeout(in); got != want {
			t.Errorf("clampPatchTimeout(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestRunPatchScan(t *testing.T) {
	// A manager returns a finalized report (Count/SecurityCount filled).
	m := &fakeManager{report: finalizePatchReport(protocol.PatchReport{
		Manager: protocol.PatchManagerApt, RebootRequired: true,
		Updates: []protocol.PatchUpdate{{Name: "openssl", Security: true}, {Name: "curl"}},
	}, fixedNow())}
	pc := newPatchCollectorWith(m)
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 1, Kind: protocol.KindPatch, Action: protocol.PatchActionScan})
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("scan result: %+v", res)
	}
	if !strings.Contains(res.Stdout, "2 update(s) pending, 1 security") || !strings.Contains(res.Stdout, "reboot required") {
		t.Fatalf("scan summary = %q", res.Stdout)
	}
	// The re-scanned report rides the next sync without another scan.
	before := m.scans
	if r := pc.Report(context.Background(), fixedNow().Add(time.Minute)); r == nil {
		t.Fatal("the report queued by a scan must be sent on the next sync")
	}
	if m.scans != before {
		t.Fatalf("the queued report must not trigger another scan: %d -> %d", before, m.scans)
	}
}

func TestRunPatchInstallDefaults(t *testing.T) {
	m := &fakeManager{installOut: "done"}
	pc := newPatchCollectorWith(m)
	// No action/mode/reboot => install, security, never.
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 2, Kind: protocol.KindPatch})
	if res.ExitCode != 0 || res.Error != "" || res.Stdout != "done" {
		t.Fatalf("install result: %+v", res)
	}
	if len(m.installs) != 1 || m.installs[0].mode != protocol.PatchModeSecurity || m.installs[0].reboot != protocol.PatchRebootNever {
		t.Fatalf("install defaults wrong: %+v", m.installs)
	}
}

func TestRunPatchInstallIDsUnknownFails(t *testing.T) {
	m := &fakeManager{report: protocol.PatchReport{Manager: protocol.PatchManagerApt, Updates: []protocol.PatchUpdate{{Name: "curl"}}}}
	pc := newPatchCollectorWith(m)
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 3, Kind: protocol.KindPatch, Mode: protocol.PatchModeIDs, IDs: []string{"curl", "nosuch"}})
	if res.ExitCode == 0 || !strings.Contains(res.Error, "not in the pending list") || !strings.Contains(res.Error, "nosuch") {
		t.Fatalf("expected unknown-id failure: %+v", res)
	}
	if len(m.installs) != 0 {
		t.Fatal("install must not run when an id is unknown")
	}
}

func TestRunPatchInstallIDsKnown(t *testing.T) {
	m := &fakeManager{
		report:     protocol.PatchReport{Manager: protocol.PatchManagerApt, Updates: []protocol.PatchUpdate{{Name: "curl"}, {Name: "openssl"}}},
		installOut: "upgraded curl",
	}
	pc := newPatchCollectorWith(m)
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 4, Kind: protocol.KindPatch, Mode: protocol.PatchModeIDs, IDs: []string{"curl"}})
	if res.ExitCode != 0 || res.Error != "" {
		t.Fatalf("install ids result: %+v", res)
	}
	if len(m.installs) != 1 || m.installs[0].mode != protocol.PatchModeIDs || len(m.installs[0].ids) != 1 || m.installs[0].ids[0] != "curl" {
		t.Fatalf("install call wrong: %+v", m.installs)
	}
}

func TestRunPatchInstallFailure(t *testing.T) {
	m := &fakeManager{installErr: context.DeadlineExceeded, installOut: "partial log"}
	pc := newPatchCollectorWith(m)
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 5, Kind: protocol.KindPatch, Mode: protocol.PatchModeAll})
	if res.ExitCode != 1 || res.Error == "" {
		t.Fatalf("expected failure: %+v", res)
	}
	if res.Stdout != "partial log" {
		t.Fatalf("install log must be kept on failure: %q", res.Stdout)
	}
}

func TestRunPatchInstallReboots(t *testing.T) {
	m := &fakeManager{installOut: "upgraded", rebooted: true}
	pc := newPatchCollectorWith(m)
	res := pc.RunPatch(context.Background(), protocol.Command{ID: 6, Kind: protocol.KindPatch, Mode: protocol.PatchModeAll, Reboot: protocol.PatchRebootIfRequired})
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "reboot initiated") {
		t.Fatalf("reboot result: %+v", res)
	}
}
