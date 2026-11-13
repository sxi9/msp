package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// roundTrip unmarshals the fixture into v, marshals it back and compares the
// generic (map[string]any) forms. Numbers are compared as float64 on both
// sides, so 476.0 and 476 are equal, but a missing, extra or renamed key, a
// null that became a value or a wrong JSON type fails the test.
func roundTrip(t *testing.T, name string, v any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: decode into %T: %v", name, v, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var want, got any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: fixture is not JSON: %v", name, err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s: re-marshaled output is not JSON: %v", name, err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%s: round trip changed the document\nfixture: %s\nmarshal: %s", name, raw, out)
	}
	return out
}

func TestFixturesRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		v    any
	}{
		{"enroll_linux.json", &EnrollRequest{}},
		{"enroll_android.json", &EnrollRequest{}},
		{"sync_linux.json", &SyncRequest{}},
		{"sync_android.json", &SyncRequest{}},
		{"sync_response.json", &SyncResponse{}},
		{"sync_response_android.json", &SyncResponse{}},
		{"command_result.json", &CommandResult{}},
		{"error.json", &ErrorResponse{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { roundTrip(t, c.name, c.v) })
	}

	// Every fixture file must be covered by a case above.
	entries, err := os.ReadDir("fixtures")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		covered := false
		for _, c := range cases {
			if c.name == e.Name() {
				covered = true
			}
		}
		if !covered {
			t.Errorf("fixture %s has no round-trip case", e.Name())
		}
	}
}

func TestFixtureSemantics(t *testing.T) {
	var el, ea EnrollRequest
	roundTrip(t, "enroll_linux.json", &el)
	roundTrip(t, "enroll_android.json", &ea)
	if el.Platform != PlatformLinux || ea.Platform != PlatformAndroid {
		t.Fatalf("platforms: %q %q", el.Platform, ea.Platform)
	}
	if !ValidPlatform(el.Platform) || !ValidPlatform(ea.Platform) {
		t.Fatal("fixture platforms must be valid")
	}
	if !strings.HasPrefix(el.EnrollmentToken, EnrollmentTokenPrefix) || len(el.EnrollmentToken) != len(EnrollmentTokenPrefix)+43 {
		t.Fatalf("enrollment token shape: %q", el.EnrollmentToken)
	}
	if ea.IntervalS != DefaultIntervalAndroid || ea.Arch != "arm64" {
		t.Fatalf("android enroll: interval %d arch %q", ea.IntervalS, ea.Arch)
	}

	var sl, sa SyncRequest
	roundTrip(t, "sync_linux.json", &sl)
	roundTrip(t, "sync_android.json", &sa)
	if sl.WaitS != MaxWaitS || sl.Facts == nil || sl.Metrics.CPUPct == nil || sl.Metrics.BatteryPct != nil {
		t.Fatalf("linux sync: wait %d facts %v cpu %v battery %v", sl.WaitS, sl.Facts, sl.Metrics.CPUPct, sl.Metrics.BatteryPct)
	}
	if sa.WaitS != 0 || sa.Facts != nil || sa.Metrics.CPUPct != nil {
		t.Fatalf("android sync: wait %d facts %v cpu %v", sa.WaitS, sa.Facts, sa.Metrics.CPUPct)
	}
	if sa.Metrics.BatteryPct == nil || *sa.Metrics.BatteryPct != 87 || sa.Metrics.BatteryCharging == nil || *sa.Metrics.BatteryCharging {
		t.Fatal("android sync battery fields")
	}
	if len(sa.Metrics.Disks) != 1 || sa.Metrics.Disks[0].Mount != "/data" {
		t.Fatalf("android disks: %+v", sa.Metrics.Disks)
	}

	var sr, sra SyncResponse
	roundTrip(t, "sync_response.json", &sr)
	roundTrip(t, "sync_response_android.json", &sra)
	if len(sr.Commands) != 2 || sr.Commands[0].Kind != KindExec || sr.Commands[1].Kind != KindScript {
		t.Fatalf("sync_response commands: %+v", sr.Commands)
	}
	if len(sra.Commands) != 3 || sra.Commands[0].Kind != KindLock || sra.Commands[1].Kind != KindPing || sra.Commands[2].Kind != KindMessage {
		t.Fatalf("sync_response_android commands: %+v", sra.Commands)
	}
	for _, c := range append(sr.Commands, sra.Commands...) {
		if err := c.Validate(); err != nil {
			t.Errorf("command %d (%s) must validate: %v", c.ID, c.Kind, err)
		}
	}
	for _, c := range sr.Commands {
		if KindSupportedOn(c.Kind, PlatformAndroid) {
			t.Errorf("kind %s must not be supported on android", c.Kind)
		}
	}
	for _, c := range sra.Commands {
		for _, p := range []string{PlatformLinux, PlatformWindows, PlatformAndroid} {
			if !KindSupportedOn(c.Kind, p) {
				t.Errorf("kind %s must be supported on %s", c.Kind, p)
			}
		}
	}

	var res CommandResult
	roundTrip(t, "command_result.json", &res)
	if res.ExitCode != 0 || res.Error != "" || res.StartedAt.IsZero() || res.StartedAt.Location().String() != "UTC" {
		t.Fatalf("command result: %+v", res)
	}

	var er ErrorResponse
	roundTrip(t, "error.json", &er)
	if er.Error != "invalid_device_token" || er.Detail != "" {
		t.Fatalf("error fixture: %+v", er)
	}
}

func TestSyncResponseCommandsNeverNull(t *testing.T) {
	out, _ := json.Marshal(SyncResponse{PollIntervalS: 30, Commands: []Command{}})
	if string(out) != `{"poll_interval_s":30,"commands":[]}` {
		t.Fatalf("got %s", out)
	}
	// Servers must initialise Commands; a nil slice would marshal as null.
	out, _ = json.Marshal(SyncResponse{PollIntervalS: 30})
	if !strings.Contains(string(out), `"commands":null`) {
		t.Fatalf("expected the nil-slice pitfall to be visible, got %s", out)
	}
}

func TestClamps(t *testing.T) {
	cases := []struct {
		n        int
		platform string
		want     int
	}{
		{0, PlatformLinux, 30}, {0, PlatformWindows, 30}, {0, PlatformAndroid, 60}, {0, "", 30},
		{2, PlatformLinux, 2}, {-5, PlatformLinux, 1}, {5000, PlatformAndroid, 3600}, {3600, PlatformLinux, 3600},
	}
	for _, c := range cases {
		if got := ClampInterval(c.n, c.platform); got != c.want {
			t.Errorf("ClampInterval(%d,%q)=%d want %d", c.n, c.platform, got, c.want)
		}
	}
	for n, want := range map[int]int{-1: 0, 0: 0, 10: 10, 25: 25, 60: 25} {
		if got := ClampWait(n); got != want {
			t.Errorf("ClampWait(%d)=%d want %d", n, got, want)
		}
	}
	for n, want := range map[int]int{0: 60, -3: 1, 1: 1, 120: 120, 3600: 3600, 9999: 3600} {
		if got := ClampTimeout(n); got != want {
			t.Errorf("ClampTimeout(%d)=%d want %d", n, got, want)
		}
	}
}

func TestPlatformTables(t *testing.T) {
	if !ValidPlatform("linux") || !ValidPlatform("windows") || !ValidPlatform("android") || ValidPlatform("darwin") || ValidPlatform("") {
		t.Fatal("ValidPlatform")
	}
	if got := ShellsFor(PlatformLinux); !reflect.DeepEqual(got, []string{"sh", "bash"}) {
		t.Fatalf("linux shells %v", got)
	}
	if got := ShellsFor(PlatformWindows); !reflect.DeepEqual(got, []string{"powershell", "cmd"}) {
		t.Fatalf("windows shells %v", got)
	}
	if got := ShellsFor(PlatformAndroid); len(got) != 0 {
		t.Fatalf("android shells %v", got)
	}
	if got := KindsFor(PlatformLinux); !reflect.DeepEqual(got, []string{"exec", "script", "lock", "ping", "message"}) {
		t.Fatalf("linux kinds %v", got)
	}
	if got := KindsFor(PlatformAndroid); !reflect.DeepEqual(got, []string{"lock", "ping", "message"}) {
		t.Fatalf("android kinds %v", got)
	}
	if KindsFor("darwin") != nil || KindSupportedOn(KindPing, "darwin") {
		t.Fatal("unknown platform must support nothing")
	}
	for _, k := range []string{KindExec, KindScript, KindLock, KindPing, KindMessage} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%s)", k)
		}
	}
	if ValidKind("shell") || ValidKind("") {
		t.Fatal("ValidKind must reject unknown kinds")
	}
}

func TestCommandValidate(t *testing.T) {
	big := strings.Repeat("x", MaxArgBytes+1)
	ok := []Command{
		{Kind: KindExec, Argv: []string{"uname", "-a"}},
		{Kind: KindExec, Argv: []string{"echo", "a;id"}, TimeoutS: 1},
		{Kind: KindExec, Argv: []string{strings.Repeat("x", MaxArgBytes)}, TimeoutS: MaxTimeoutS},
		{Kind: KindScript, Shell: ShellSh, Body: "df -h\n", TimeoutS: 120},
		{Kind: KindScript, Shell: ShellBash, Body: "x"},
		{Kind: KindScript, Shell: ShellPowershell, Body: "Get-Date"},
		{Kind: KindScript, Shell: ShellCmd, Body: "dir"},
		{Kind: KindScript, Shell: ShellSh, Body: strings.Repeat("y", MaxScriptBytes)},
		{Kind: KindLock, TimeoutS: 30},
		{Kind: KindPing},
		{Kind: KindMessage, Text: "Please call the helpdesk", TimeoutS: 30},
	}
	for i, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("ok[%d] %+v: unexpected error %v", i, c, err)
		}
	}
	bad := []struct {
		c    Command
		want string
	}{
		{Command{Kind: KindExec}, "at least one"},
		{Command{Kind: KindExec, Argv: []string{}}, "at least one"},
		{Command{Kind: KindExec, Argv: []string{"ls", big}}, "argv[1] exceeds"},
		{Command{Kind: KindExec, Argv: []string{"ls"}, Shell: "sh"}, "must not set shell"},
		{Command{Kind: KindExec, Argv: []string{"ls"}, Body: "x"}, "must not set shell"},
		{Command{Kind: KindExec, Argv: []string{"ls"}, Text: "x"}, "must not set text"},
		{Command{Kind: KindExec, Argv: []string{"ls"}, TimeoutS: -1}, "timeout_s"},
		{Command{Kind: KindExec, Argv: []string{"ls"}, TimeoutS: MaxTimeoutS + 1}, "timeout_s"},
		{Command{Kind: KindScript, Shell: "zsh", Body: "x"}, "shell must be"},
		{Command{Kind: KindScript, Body: "x"}, "shell must be"},
		{Command{Kind: KindScript, Shell: ShellSh}, "body must not be empty"},
		{Command{Kind: KindScript, Shell: ShellSh, Body: strings.Repeat("y", MaxScriptBytes+1)}, "body exceeds"},
		{Command{Kind: KindScript, Shell: ShellSh, Body: "x", Argv: []string{"a"}}, "must not set argv"},
		{Command{Kind: KindScript, Shell: ShellSh, Body: "x", Text: "t"}, "must not set text"},
		{Command{Kind: KindLock, Argv: []string{"a"}}, "takes no"},
		{Command{Kind: KindPing, Text: "x"}, "takes no"},
		{Command{Kind: KindLock, Body: "x"}, "takes no"},
		{Command{Kind: KindMessage}, "text must not be empty"},
		{Command{Kind: KindMessage, Text: strings.Repeat("m", MaxMessageBytes+1)}, "text exceeds"},
		{Command{Kind: KindMessage, Text: "hi", Argv: []string{"a"}}, "must not set argv"},
		{Command{Kind: KindMessage, Text: "hi", Shell: "sh"}, "must not set argv"},
		{Command{Kind: "shell", Argv: []string{"ls"}}, "unknown kind"},
		{Command{}, "unknown kind"},
	}
	for i, b := range bad {
		err := b.c.Validate()
		if err == nil {
			t.Errorf("bad[%d] %+v: expected error containing %q", i, b.c, b.want)
			continue
		}
		if !strings.Contains(err.Error(), b.want) {
			t.Errorf("bad[%d]: error %q does not contain %q", i, err, b.want)
		}
	}
}

func TestCommandJSONShape(t *testing.T) {
	cases := map[string]Command{
		`{"id":7,"kind":"exec","argv":["uname","-a"],"timeout_s":60}`:                    {ID: 7, Kind: KindExec, Argv: []string{"uname", "-a"}, TimeoutS: 60},
		`{"id":8,"kind":"script","shell":"sh","body":"df -h\nuptime\n","timeout_s":120}`: {ID: 8, Kind: KindScript, Shell: ShellSh, Body: "df -h\nuptime\n", TimeoutS: 120},
		`{"id":9,"kind":"lock","timeout_s":30}`:                                          {ID: 9, Kind: KindLock, TimeoutS: 30},
		`{"id":10,"kind":"ping","timeout_s":30}`:                                         {ID: 10, Kind: KindPing, TimeoutS: 30},
		`{"id":11,"kind":"message","text":"Please call the helpdesk","timeout_s":30}`:    {ID: 11, Kind: KindMessage, Text: "Please call the helpdesk", TimeoutS: 30},
	}
	for want, c := range cases {
		out, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Errorf("marshal %+v\n got %s\nwant %s", c, out, want)
		}
	}
}
