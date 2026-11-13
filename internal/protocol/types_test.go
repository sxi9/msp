package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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
		{"sync_rustdesk.json", &SyncRequest{}},
		{"sync_response_rustdesk.json", &SyncResponse{}},
		{"sync_android_apps.json", &SyncRequest{}},
		{"command_result.json", &CommandResult{}},
		{"command_shell.json", &Command{}},
		{"sync_response_shell.json", &SyncResponse{}},
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

	// RD1: a sync whose facts carry the RustDesk id, and a response that
	// tells the agent which relay to use. The plain fixtures carry neither.
	var srd SyncRequest
	roundTrip(t, "sync_rustdesk.json", &srd)
	if srd.Facts == nil || srd.Facts.RustdeskID != "123456789" || !ValidRustdeskID(srd.Facts.RustdeskID) || srd.Facts.Validate() != nil {
		t.Fatalf("sync_rustdesk facts: %+v", srd.Facts)
	}
	if sl.Facts.RustdeskID != "" || sr.Rustdesk != nil || sra.Rustdesk != nil {
		t.Fatal("plain fixtures must not carry rustdesk fields")
	}
	var srr SyncResponse
	roundTrip(t, "sync_response_rustdesk.json", &srr)
	if srr.Rustdesk == nil || srr.Rustdesk.Host != "rmm.example.com" || srr.Rustdesk.Key == "" || len(srr.Commands) != 0 {
		t.Fatalf("sync_response_rustdesk: %+v", srr)
	}

	// Installed-apps inventory: an Android sync whose facts carry apps, in
	// the order the agent sends them (non-system first, then by label). The
	// plain fixtures carry no apps.
	var sapps SyncRequest
	roundTrip(t, "sync_android_apps.json", &sapps)
	if sapps.Facts == nil || len(sapps.Facts.Apps) != 5 || sapps.Facts.Validate() != nil {
		t.Fatalf("sync_android_apps facts: %+v", sapps.Facts)
	}
	apps := sapps.Facts.Apps
	if apps[2].Package != "com.sxi9.msp.agent" || apps[2].Label != "MSP Agent" || apps[2].Version != "0.1.0" || apps[2].System {
		t.Fatalf("apps[2] = %+v", apps[2])
	}
	if apps[0].Label != apps[0].Package || apps[0].Version != "" {
		t.Fatalf("apps[0] must show the label fallback and an empty version: %+v", apps[0])
	}
	if !apps[4].System || apps[4].Package != "com.android.settings" {
		t.Fatalf("apps[4] = %+v", apps[4])
	}
	for i := 1; i < len(apps); i++ {
		if apps[i-1].System && !apps[i].System {
			t.Fatalf("non-system apps must come first: %+v", apps)
		}
		if apps[i-1].System == apps[i].System && strings.ToLower(apps[i-1].Label) > strings.ToLower(apps[i].Label) {
			t.Fatalf("apps must be sorted by label within a group: %+v", apps)
		}
	}
	if sl.Facts.Apps != nil || sa.Facts != nil || srd.Facts.Apps != nil || el.Facts.Apps != nil || ea.Facts.Apps != nil {
		t.Fatal("plain fixtures must not carry apps")
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
	if got := KindsFor(PlatformLinux); !reflect.DeepEqual(got, []string{"exec", "script", "lock", "ping", "message", "shell"}) {
		t.Fatalf("linux kinds %v", got)
	}
	if got := KindsFor(PlatformAndroid); !reflect.DeepEqual(got, []string{"lock", "ping", "message"}) {
		t.Fatalf("android kinds %v", got)
	}
	if KindsFor("darwin") != nil || KindSupportedOn(KindPing, "darwin") {
		t.Fatal("unknown platform must support nothing")
	}
	for _, k := range []string{KindExec, KindScript, KindLock, KindPing, KindMessage, KindShell} {
		if !ValidKind(k) {
			t.Errorf("ValidKind(%s)", k)
		}
	}
	if ValidKind("") || ValidKind("desktop") {
		t.Fatal("ValidKind must reject unknown kinds")
	}
	// shell (RS1) is desktop only.
	if !KindSupportedOn(KindShell, PlatformLinux) || !KindSupportedOn(KindShell, PlatformWindows) {
		t.Fatal("shell must be supported on desktop platforms")
	}
	if KindSupportedOn(KindShell, PlatformAndroid) {
		t.Fatal("shell must not be supported on android")
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
		{Kind: KindShell},
		{Kind: KindShell, Shell: ShellBash, Cols: 120, Rows: 32, TimeoutS: DefaultShellTimeoutS},
		{Kind: KindShell, Shell: ShellPowershell, TimeoutS: MaxShellTimeoutS},
		// cols/rows are clamped by the server, never rejected, so even an
		// out-of-range or negative size is a valid command shape.
		{Kind: KindShell, Shell: ShellSh, Cols: 9000, Rows: -3},
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
		{Command{Kind: KindShell, Argv: []string{"ls"}}, "shell must not set argv"},
		{Command{Kind: KindShell, Body: "x"}, "shell must not set argv"},
		{Command{Kind: KindShell, Text: "x"}, "shell must not set argv"},
		{Command{Kind: KindShell, Shell: "zsh"}, "shell must be empty"},
		{Command{Kind: KindShell, TimeoutS: MaxShellTimeoutS + 1}, "timeout_s"},
		{Command{Kind: "telnet", Argv: []string{"ls"}}, "unknown kind"},
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

func TestRustdesk(t *testing.T) {
	for _, ok := range []string{"1", "123456789", strings.Repeat("9", MaxRustdeskIDLen)} {
		if !ValidRustdeskID(ok) {
			t.Errorf("ValidRustdeskID(%q) must be true", ok)
		}
		if err := (Facts{RustdeskID: ok}).Validate(); err != nil {
			t.Errorf("Facts{%q}.Validate: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "12a", "-1", "1 2", "１２", strings.Repeat("9", MaxRustdeskIDLen+1), "123456789?password=x"} {
		if ValidRustdeskID(bad) {
			t.Errorf("ValidRustdeskID(%q) must be false", bad)
		}
	}
	if err := (Facts{}).Validate(); err != nil {
		t.Fatalf("empty rustdesk_id is allowed: %v", err)
	}
	if err := (Facts{RustdeskID: "x"}).Validate(); err == nil || !strings.Contains(err.Error(), "rustdesk_id") {
		t.Fatalf("Facts{x}.Validate = %v", err)
	}
	if got := RustdeskConnectURL("123456789"); got != "rustdesk://connect/123456789" {
		t.Fatalf("RustdeskConnectURL = %q", got)
	}

	// Relay host: a bare hostname or IP; key: one base64 line. Anything that
	// would need quoting in RustDesk2.toml or splits on the hbbs command
	// line is refused on the server and on the agent.
	for _, ok := range []string{"rmm.example.com", "RMM.Example.COM", "rmm", "10.0.0.1", "2001:db8::1", "a-b.c1", strings.Repeat("a", 63) + ".example"} {
		if !ValidRustdeskHost(ok) {
			t.Errorf("ValidRustdeskHost(%q) must be true", ok)
		}
	}
	for _, bad := range []string{"", "rmm.example.com:21116", "10.0.0.1:21116", "x.example -k evil", "https://rmm.example.com", "rmm.example.com/", "-a.example", "a-.example", "a..b", "x.example'\nkey = 'other'", "rmm.example.com\n", strings.Repeat("a", 64) + ".example", strings.Repeat("a.", 127) + "aaaa"} {
		if ValidRustdeskHost(bad) {
			t.Errorf("ValidRustdeskHost(%q) must be false", bad)
		}
	}
	for _, ok := range []string{"OeVuKk5nlHiXp+7qsR6CbcHBGxhU4PJkNbvoX3P5X0k=", "KEY=", "k", "abc-_", strings.Repeat("A", MaxRustdeskKeyLen)} {
		if !ValidRustdeskKey(ok) {
			t.Errorf("ValidRustdeskKey(%q) must be true", ok)
		}
	}
	for _, bad := range []string{"", "KEY=\n", "KE Y=", "KEY='x'", "KEY=\"", "ключ", strings.Repeat("A", MaxRustdeskKeyLen+1)} {
		if ValidRustdeskKey(bad) {
			t.Errorf("ValidRustdeskKey(%q) must be false", bad)
		}
	}
	if err := (Rustdesk{Host: "rmm.example.com", Key: "KEY="}).Validate(); err != nil {
		t.Fatalf("valid relay: %v", err)
	}
	if err := (Rustdesk{Host: "rmm.example.com:21116", Key: "KEY="}).Validate(); err == nil || !strings.Contains(err.Error(), "port") {
		t.Fatalf("host with port: %v", err)
	}
	if err := (Rustdesk{Host: "rmm.example.com", Key: "bad key"}).Validate(); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("bad key: %v", err)
	}

	// Omitted when nil, present when set; facts omit an empty id.
	out, _ := json.Marshal(SyncResponse{PollIntervalS: 30, Commands: []Command{}})
	if string(out) != `{"poll_interval_s":30,"commands":[]}` {
		t.Fatalf("got %s", out)
	}
	out, _ = json.Marshal(EnrollResponse{DeviceID: 1, DeviceToken: "dt_x", PollIntervalS: 30, Rustdesk: &Rustdesk{Host: "h", Key: "k"}})
	if string(out) != `{"device_id":1,"device_token":"dt_x","poll_interval_s":30,"rustdesk":{"host":"h","key":"k"}}` {
		t.Fatalf("got %s", out)
	}
	out, _ = json.Marshal(Facts{IPs: []string{}})
	if strings.Contains(string(out), "rustdesk_id") {
		t.Fatalf("empty rustdesk_id must be omitted: %s", out)
	}
}

func TestFactsAppsValidate(t *testing.T) {
	app := func(pkg, label, version string, system bool) App {
		return App{Package: pkg, Label: label, Version: version, System: system}
	}
	ok := []Facts{
		{},
		{Apps: []App{}},
		{Apps: []App{app("a", "", "", false)}},
		{Apps: []App{app(strings.Repeat("p", MaxAppPackageLen), strings.Repeat("l", MaxAppLabelLen), strings.Repeat("v", MaxAppVersionLen), true)}},
		{Apps: make([]App, MaxApps)},
	}
	for i := range ok[4].Apps {
		ok[4].Apps[i] = app("com.example.a", "A", "1", i%2 == 0)
	}
	for i, f := range ok {
		if err := f.Validate(); err != nil {
			t.Errorf("ok[%d]: unexpected error %v", i, err)
		}
	}
	tooMany := make([]App, MaxApps+1)
	for i := range tooMany {
		tooMany[i] = app("com.example.a", "A", "1", false)
	}
	bad := []struct {
		f    Facts
		want string
	}{
		{Facts{Apps: []App{app("ok", "", "", false), app("", "No package", "1", false)}}, "facts.apps[1].package must be 1..256"},
		{Facts{Apps: []App{app(strings.Repeat("p", MaxAppPackageLen+1), "", "", false)}}, "facts.apps[0].package must be 1..256"},
		{Facts{Apps: []App{app("a", "", "", false), app("b", "", "", false), app("c", strings.Repeat("l", MaxAppLabelLen+1), "", false)}}, "facts.apps[2].label exceeds 256"},
		{Facts{Apps: []App{app("a", "", strings.Repeat("v", MaxAppVersionLen+1), false)}}, "facts.apps[0].version exceeds 64"},
		{Facts{Apps: tooMany}, "facts.apps has 1001 entries, at most 1000"},
		{Facts{RustdeskID: "x", Apps: []App{app("", "", "", false)}}, "rustdesk_id"},
	}
	for i, b := range bad {
		err := b.f.Validate()
		if err == nil {
			t.Errorf("bad[%d]: expected error containing %q", i, b.want)
			continue
		}
		if !strings.Contains(err.Error(), b.want) {
			t.Errorf("bad[%d]: error %q does not contain %q", i, err, b.want)
		}
	}

	// Omitted when nil or empty; present with exactly the four keys otherwise.
	out, _ := json.Marshal(Facts{IPs: []string{}, Apps: []App{}})
	if strings.Contains(string(out), "apps") {
		t.Fatalf("empty apps must be omitted: %s", out)
	}
	out, _ = json.Marshal(App{Package: "com.example", Label: "Example", Version: "1.2.3", System: true})
	if string(out) != `{"package":"com.example","label":"Example","version":"1.2.3","system":true}` {
		t.Fatalf("app JSON shape: %s", out)
	}
}

func TestShellClamps(t *testing.T) {
	for n, want := range map[int]int{0: DefaultShellCols, -5: MinShellCols, 10: MinShellCols, 20: 20, 300: 300, 500: 500, 9000: MaxShellCols} {
		if got := ClampShellCols(n); got != want {
			t.Errorf("ClampShellCols(%d)=%d want %d", n, got, want)
		}
	}
	for n, want := range map[int]int{0: DefaultShellRows, -1: MinShellRows, 3: MinShellRows, 5: 5, 100: 100, 200: 200, 9000: MaxShellRows} {
		if got := ClampShellRows(n); got != want {
			t.Errorf("ClampShellRows(%d)=%d want %d", n, got, want)
		}
	}
	for n, want := range map[int]int{0: DefaultShellTimeoutS, -3: 1, 1: 1, 3600: 3600, 14400: 14400, 28800: 28800, 99999: MaxShellTimeoutS} {
		if got := ClampShellTimeout(n); got != want {
			t.Errorf("ClampShellTimeout(%d)=%d want %d", n, got, want)
		}
	}
}

func TestShellControlFrames(t *testing.T) {
	// Each constructor must marshal to exactly the wire JSON of RS1 section B.
	cases := []struct {
		frame ControlFrame
		want  string
	}{
		{ResizeFrame(120, 32), `{"type":"resize","cols":120,"rows":32}`},
		{ExitFrame(0), `{"type":"exit","code":0}`},
		{ExitFrame(130), `{"type":"exit","code":130}`},
		{StatusFrame(ShellStateWaitingForAgent, ""), `{"type":"status","state":"waiting_for_agent"}`},
		{StatusFrame(ShellStateConnected, ""), `{"type":"status","state":"connected"}`},
		{StatusFrame(ShellStateClosed, CloseIdleTimeout), `{"type":"status","state":"closed","reason":"idle_timeout"}`},
	}
	for _, c := range cases {
		if got := string(c.frame.Encode()); got != c.want {
			t.Errorf("Encode %+v\n got %s\nwant %s", c.frame, got, c.want)
		}
		// Round trip: every emitted frame parses back to an equal value.
		got, err := ParseControlFrame([]byte(c.want))
		if err != nil {
			t.Errorf("ParseControlFrame(%s): %v", c.want, err)
			continue
		}
		if !reflect.DeepEqual(got, c.frame) {
			t.Errorf("ParseControlFrame(%s) = %+v want %+v", c.want, got, c.frame)
		}
	}

	// A resize frame exposes its dimensions; an exit frame its code.
	r, err := ParseControlFrame([]byte(`{"type":"resize","cols":200,"rows":50}`))
	if err != nil || r.Type != CtrlResize || r.Cols != 200 || r.Rows != 50 {
		t.Fatalf("resize parse: %+v err %v", r, err)
	}
	e, err := ParseControlFrame([]byte(`{"type":"exit","code":0}`))
	if err != nil || e.Type != CtrlExit || e.Code == nil || *e.Code != 0 {
		t.Fatalf("exit parse: %+v err %v", e, err)
	}

	// Unknown and malformed frames are rejected.
	for _, bad := range []string{`{"type":"bogus"}`, `{"type":""}`, `{}`, `not json`} {
		if _, err := ParseControlFrame([]byte(bad)); err == nil {
			t.Errorf("ParseControlFrame(%q) must fail", bad)
		}
	}
}

func TestShellCloseReasons(t *testing.T) {
	for _, ok := range []string{"", CloseTechnicianClosed, CloseAgentClosed, CloseIdleTimeout, CloseMaxDuration, CloseServerShutdown, CloseAgentNeverConnected} {
		if !ValidCloseReason(ok) {
			t.Errorf("ValidCloseReason(%q) must be true", ok)
		}
	}
	for _, bad := range []string{"closed", "technician", "idle", "killed"} {
		if ValidCloseReason(bad) {
			t.Errorf("ValidCloseReason(%q) must be false", bad)
		}
	}
}

func TestShellSessionJSON(t *testing.T) {
	// connected_at is null until the agent attaches; the key is always present.
	out, _ := json.Marshal(ShellSession{})
	if string(out) != `{"bytes_in":0,"bytes_out":0,"close_reason":"","transcript_truncated":false,"connected_at":null}` {
		t.Fatalf("zero ShellSession: %s", out)
	}
	at := time.Date(2026, 11, 13, 10, 0, 3, 0, time.UTC)
	out, _ = json.Marshal(ShellSession{BytesIn: 1, BytesOut: 2, CloseReason: CloseAgentClosed, TranscriptTruncated: true, ConnectedAt: &at})
	if string(out) != `{"bytes_in":1,"bytes_out":2,"close_reason":"agent_closed","transcript_truncated":true,"connected_at":"2026-11-13T10:00:03Z"}` {
		t.Fatalf("populated ShellSession: %s", out)
	}

	// A shell COMMAND with no session carries no shell_session key; cols/rows
	// at the default are omitted too.
	out, _ = json.Marshal(Command{ID: 42, Kind: KindShell, Shell: ShellBash, TimeoutS: DefaultShellTimeoutS})
	if string(out) != `{"id":42,"kind":"shell","shell":"bash","timeout_s":14400}` {
		t.Fatalf("shell command without session: %s", out)
	}
}

func TestShellFixtures(t *testing.T) {
	var cmd Command
	roundTrip(t, "command_shell.json", &cmd)
	if cmd.Kind != KindShell || cmd.Shell != ShellBash || cmd.Cols != 120 || cmd.Rows != 32 || cmd.TimeoutS != DefaultShellTimeoutS {
		t.Fatalf("command_shell: %+v", cmd)
	}
	if err := cmd.Validate(); err != nil {
		t.Fatalf("command_shell must validate: %v", err)
	}
	if cmd.ShellSession == nil || cmd.ShellSession.BytesIn != 128 || cmd.ShellSession.BytesOut != 4096 ||
		cmd.ShellSession.CloseReason != CloseTechnicianClosed || cmd.ShellSession.TranscriptTruncated ||
		cmd.ShellSession.ConnectedAt == nil || !ValidCloseReason(cmd.ShellSession.CloseReason) {
		t.Fatalf("command_shell session: %+v", cmd.ShellSession)
	}

	var sr SyncResponse
	roundTrip(t, "sync_response_shell.json", &sr)
	if len(sr.Commands) != 1 || sr.Commands[0].Kind != KindShell || sr.Commands[0].ShellSession != nil {
		t.Fatalf("sync_response_shell: %+v", sr.Commands)
	}
	if err := sr.Commands[0].Validate(); err != nil {
		t.Fatalf("sync_response_shell command must validate: %v", err)
	}
	// Desktop-only: supported on linux/windows, not android.
	if !KindSupportedOn(KindShell, PlatformLinux) || KindSupportedOn(KindShell, PlatformAndroid) {
		t.Fatal("shell platform support")
	}
}
