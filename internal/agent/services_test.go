//go:build linux

package agent

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// fakeProber is an injectable serviceProber for ServiceCollector tests.
type fakeProber struct {
	calls [][]protocol.WatchItem
	fn    func([]protocol.WatchItem) []protocol.ServiceState
}

func (f *fakeProber) Probe(ctx context.Context, items []protocol.WatchItem) []protocol.ServiceState {
	f.calls = append(f.calls, items)
	if f.fn != nil {
		return f.fn(items)
	}
	return notRunning(items)
}

func TestParseIsActive(t *testing.T) {
	data, err := os.ReadFile("testdata/systemctl-is-active.txt")
	if err != nil {
		t.Fatal(err)
	}
	got := parseIsActive(string(data), 4)
	want := []bool{true, false, true, false}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIsActive = %v, want %v", got, want)
	}
	// "activating" and other transitional states are not running; only "active" is.
	if got := parseIsActive("activating\nactive\n", 2); !reflect.DeepEqual(got, []bool{false, true}) {
		t.Fatalf("transitional: got %v", got)
	}
	// A short output (systemctl errored mid-stream) leaves the missing units down.
	if got := parseIsActive("active\n", 3); !reflect.DeepEqual(got, []bool{true, false, false}) {
		t.Fatalf("short output: got %v", got)
	}
	// No output at all: every unit down.
	if got := parseIsActive("", 2); !reflect.DeepEqual(got, []bool{false, false}) {
		t.Fatalf("empty output: got %v", got)
	}
}

func TestLinuxServiceProber(t *testing.T) {
	active, err := os.ReadFile("testdata/systemctl-is-active.txt")
	if err != nil {
		t.Fatal(err)
	}
	var systemctlArgs []string
	fx := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		switch c.name {
		case "systemctl":
			systemctlArgs = c.args
			return cmdResult{Stdout: string(active), ExitCode: 3}, nil // non-zero is normal when a unit is down
		case "pgrep":
			// args: -x <name>; "backup" runs, anything else does not.
			if len(c.args) == 2 && c.args[1] == "backup" {
				return cmdResult{ExitCode: 0}, nil
			}
			return cmdResult{ExitCode: 1}, nil
		}
		t.Fatalf("unexpected command %q", c.name)
		return cmdResult{}, nil
	}}
	p := newPlatformServiceProber(fx)
	items := []protocol.WatchItem{
		{Kind: protocol.WatchKindService, Name: "ssh.service"},
		{Kind: protocol.WatchKindProcess, Name: "backup"},
		{Kind: protocol.WatchKindService, Name: "nginx.service"},
		{Kind: protocol.WatchKindService, Name: "cron.service"},
		{Kind: protocol.WatchKindProcess, Name: "gone"},
		{Kind: protocol.WatchKindService, Name: "bogus.service"},
	}
	got := p.Probe(context.Background(), items)
	// systemctl is called once with is-active then the four service names in order.
	wantArgs := []string{"is-active", "ssh.service", "nginx.service", "cron.service", "bogus.service"}
	if !reflect.DeepEqual(systemctlArgs, wantArgs) {
		t.Fatalf("systemctl args = %v, want %v", systemctlArgs, wantArgs)
	}
	want := []protocol.ServiceState{
		{Kind: protocol.WatchKindService, Name: "ssh.service", Running: true},
		{Kind: protocol.WatchKindProcess, Name: "backup", Running: true},
		{Kind: protocol.WatchKindService, Name: "nginx.service", Running: false},
		{Kind: protocol.WatchKindService, Name: "cron.service", Running: true},
		{Kind: protocol.WatchKindProcess, Name: "gone", Running: false},
		{Kind: protocol.WatchKindService, Name: "bogus.service", Running: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Probe = %+v\nwant %+v", got, want)
	}
}

func TestLinuxProberSystemctlMissing(t *testing.T) {
	// systemctl cannot start: every service reports not running (safe default).
	fx := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if c.name == "systemctl" {
			return cmdResult{ExitCode: -1}, os.ErrNotExist
		}
		return cmdResult{ExitCode: 1}, nil
	}}
	p := newPlatformServiceProber(fx)
	got := p.Probe(context.Background(), []protocol.WatchItem{{Kind: protocol.WatchKindService, Name: "ssh.service"}})
	if len(got) != 1 || got[0].Running {
		t.Fatalf("expected ssh not running on systemctl failure, got %+v", got)
	}
}

func TestServiceCollectorSetWatchAndCollect(t *testing.T) {
	probe := &fakeProber{fn: func(items []protocol.WatchItem) []protocol.ServiceState {
		out := make([]protocol.ServiceState, len(items))
		for i, it := range items {
			out[i] = protocol.ServiceState{Kind: it.Kind, Name: it.Name, Running: it.Name == "ssh.service"}
		}
		return out
	}}
	c := newServiceCollectorWith(probe)

	// No watch set yet: Collect returns nil and never probes.
	if got := c.Collect(context.Background()); got != nil {
		t.Fatalf("expected nil before SetWatch, got %v", got)
	}
	if len(probe.calls) != 0 {
		t.Fatalf("prober called with no watch")
	}

	// SetWatch drops invalid items (bad kind, blank name, control char, over-length)
	// and trims whitespace.
	c.SetWatch([]protocol.WatchItem{
		{Kind: protocol.WatchKindService, Name: "  ssh.service "},
		{Kind: "daemon", Name: "bad-kind"},
		{Kind: protocol.WatchKindProcess, Name: ""},
		{Kind: protocol.WatchKindService, Name: "nginx\n.service"},
		{Kind: protocol.WatchKindProcess, Name: "backup"},
	})
	got := c.Collect(context.Background())
	want := []protocol.ServiceState{
		{Kind: protocol.WatchKindService, Name: "ssh.service", Running: true},
		{Kind: protocol.WatchKindProcess, Name: "backup", Running: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Collect = %+v\nwant %+v", got, want)
	}

	// An empty watch clears it: Collect returns nil again.
	c.SetWatch(nil)
	if got := c.Collect(context.Background()); got != nil {
		t.Fatalf("expected nil after clearing watch, got %v", got)
	}
}

func TestServiceCollectorCaps(t *testing.T) {
	// SetWatch caps at MaxWatchItems; Collect caps at MaxServices.
	big := make([]protocol.WatchItem, protocol.MaxWatchItems+50)
	for i := range big {
		big[i] = protocol.WatchItem{Kind: protocol.WatchKindService, Name: "svc"}
	}
	probe := &fakeProber{}
	c := newServiceCollectorWith(probe)
	c.SetWatch(big)
	got := c.Collect(context.Background())
	if len(got) != protocol.MaxServices {
		t.Fatalf("Collect returned %d, want cap %d", len(got), protocol.MaxServices)
	}
	if n := len(probe.calls[0]); n != protocol.MaxWatchItems {
		t.Fatalf("watch stored %d items, want cap %d", n, protocol.MaxWatchItems)
	}
	if err := (protocol.Facts{Services: got}).Validate(); err != nil {
		t.Fatalf("collected services fail Facts.Validate: %v", err)
	}
}

func TestParseWinServiceStates(t *testing.T) {
	data, err := os.ReadFile("testdata/win-service-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	items := []protocol.WatchItem{
		{Kind: protocol.WatchKindService, Name: "SSH"},         // case-insensitive -> false
		{Kind: protocol.WatchKindService, Name: "nginx"},       // -> true
		{Kind: protocol.WatchKindService, Name: "missing"},     // absent -> false
		{Kind: protocol.WatchKindProcess, Name: "backup"},      // -> true
		{Kind: protocol.WatchKindProcess, Name: "notepad.exe"}, // trim .exe -> notepad -> true
		{Kind: protocol.WatchKindProcess, Name: "gone"},        // -> false
	}
	got := parseWinServiceStates(data, items)
	want := []protocol.ServiceState{
		{Kind: protocol.WatchKindService, Name: "SSH", Running: false},
		{Kind: protocol.WatchKindService, Name: "nginx", Running: true},
		{Kind: protocol.WatchKindService, Name: "missing", Running: false},
		{Kind: protocol.WatchKindProcess, Name: "backup", Running: true},
		{Kind: protocol.WatchKindProcess, Name: "notepad.exe", Running: true},
		{Kind: protocol.WatchKindProcess, Name: "gone", Running: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseWinServiceStates = %+v\nwant %+v", got, want)
	}
	// A nil/garbled snapshot reports every item not running.
	none := parseWinServiceStates(nil, items)
	for _, s := range none {
		if s.Running {
			t.Fatalf("expected all not-running for nil snapshot, got %+v", none)
		}
	}
}
