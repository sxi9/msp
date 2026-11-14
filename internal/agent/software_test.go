//go:build linux

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxi9/msp/internal/protocol"
)

// TestParseDpkgFixture parses captured dpkg-query output and checks the fields,
// the maintainer-to-publisher cleanup and that malformed lines are skipped.
func TestParseDpkgFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "dpkg-query.txt"))
	if err != nil {
		t.Fatal(err)
	}
	items := parseDpkg(string(b))
	if len(items) != 6 {
		t.Fatalf("want 6 items, got %d: %+v", len(items), items)
	}
	byName := map[string]protocol.SoftwareItem{}
	for _, it := range items {
		if it.Source != protocol.SoftwareSourceDpkg {
			t.Fatalf("source = %q, want dpkg", it.Source)
		}
		byName[it.Name] = it
	}
	if c := byName["curl"]; c.Version != "8.5.0-2ubuntu10.6" || c.Publisher != "Ubuntu Developers" {
		t.Fatalf("curl = %+v", c)
	}
	if f := byName["firefox"]; f.Publisher != "Mozilla" {
		t.Fatalf("firefox publisher = %q (the <email> must be stripped)", f.Publisher)
	}
	// A maintainer with no email is kept verbatim.
	if z := byName["zlib1g:amd64"]; z.Publisher != "Ubuntu Developers" || z.Version != "1:1.3.dfsg-3.1ubuntu2.1" {
		t.Fatalf("zlib = %+v", z)
	}
	if _, ok := byName["broken-no-version-line"]; ok {
		t.Fatal("a line with no version must be skipped")
	}
}

// TestParseRpm checks the rpm format parse and the "(none)" vendor cleanup.
func TestParseRpm(t *testing.T) {
	out := strings.Join([]string{
		"bash\t5.2.15-5.el9\tRocky Enterprise Software Foundation",
		"filesystem\t3.16-5.el9\t(none)",
		"\t1.0\tOrphan", // no name: skipped
		"openssl\t3.0.7-18.el9_2\tRocky Enterprise Software Foundation",
	}, "\n")
	items := parseRpm(out)
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %d: %+v", len(items), items)
	}
	if items[0].Name != "bash" || items[0].Source != protocol.SoftwareSourceRPM {
		t.Fatalf("items[0] = %+v", items[0])
	}
	if items[1].Name != "filesystem" || items[1].Publisher != "" {
		t.Fatalf("(none) vendor must become empty publisher: %+v", items[1])
	}
}

// TestParseRegistryFixture parses the captured uninstall-key dump and checks the
// selection: real programs are kept; a system component, a nameless entry, two
// updates and a child entry are dropped.
func TestParseRegistryFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "registry-uninstall.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw []rawUninstallEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("fixture not JSON: %v", err)
	}
	items := parseRegistry(raw)
	if len(items) != 4 {
		t.Fatalf("want 4 items, got %d: %+v", len(items), items)
	}
	names := map[string]bool{}
	for _, it := range items {
		if it.Source != protocol.SoftwareSourceRegistry {
			t.Fatalf("source = %q, want registry", it.Source)
		}
		names[it.Name] = true
	}
	for _, want := range []string{"Mozilla Firefox (x64 en-US)", "7-Zip 24.08 (x64)", "Google Chrome", "Microsoft Visual C++ 2015-2022 Redistributable (x64)"} {
		if !names[want] {
			t.Fatalf("missing expected program %q in %v", want, names)
		}
	}
	for _, drop := range []string{"Windows SDK AddOn", "Security Update for Windows (KB5040442)", "Update for Microsoft Office (KB5002640)", "KB5041580", ""} {
		if names[drop] {
			t.Fatalf("entry %q should have been dropped", drop)
		}
	}
}

// TestFinalizeSoftwareSortCapBudget checks the shared normalisation: fields are
// trimmed and length-bounded (rune-safe), the list is sorted by lower-cased name,
// and both the entry cap and the byte budget are enforced.
func TestFinalizeSoftwareSortCapBudget(t *testing.T) {
	// Sort is case-insensitive by name; a blank name is dropped.
	in := []protocol.SoftwareItem{
		{Name: "zlib", Source: protocol.SoftwareSourceDpkg},
		{Name: "  Apt  ", Version: " 2.7 ", Publisher: " Ubuntu ", Source: protocol.SoftwareSourceDpkg},
		{Name: "", Source: protocol.SoftwareSourceDpkg},
		{Name: "curl", Source: protocol.SoftwareSourceDpkg},
	}
	out := finalizeSoftware(in)
	if len(out) != 3 {
		t.Fatalf("want 3 (blank dropped), got %d: %+v", len(out), out)
	}
	if out[0].Name != "Apt" || out[0].Version != "2.7" || out[0].Publisher != "Ubuntu" {
		t.Fatalf("trim/sort: out[0] = %+v", out[0])
	}
	if out[1].Name != "curl" || out[2].Name != "zlib" {
		t.Fatalf("order: %+v", out)
	}

	// Field length bounds, rune-safe (a multibyte publisher cut on a boundary).
	long := []protocol.SoftwareItem{{
		Name:      strings.Repeat("n", protocol.MaxSoftwareNameLen+50),
		Version:   strings.Repeat("v", protocol.MaxSoftwareVersionLen+50),
		Publisher: strings.Repeat("é", protocol.MaxSoftwarePublisherLen), // 2 bytes each => way over
		Source:    protocol.SoftwareSourceDpkg,
	}}
	lo := finalizeSoftware(long)[0]
	if len(lo.Name) != protocol.MaxSoftwareNameLen || len(lo.Version) != protocol.MaxSoftwareVersionLen {
		t.Fatalf("length bounds: name %d version %d", len(lo.Name), len(lo.Version))
	}
	if len(lo.Publisher) > protocol.MaxSoftwarePublisherLen {
		t.Fatalf("publisher over bound: %d", len(lo.Publisher))
	}
	if !utf8Valid(lo.Publisher) {
		t.Fatal("publisher cut split a rune")
	}

	// Entry cap: more than MaxSoftware entries is cut to MaxSoftware.
	many := make([]protocol.SoftwareItem, protocol.MaxSoftware+25)
	for i := range many {
		many[i] = protocol.SoftwareItem{Name: fmt.Sprintf("pkg-%05d", i), Version: "1", Source: protocol.SoftwareSourceDpkg}
	}
	capped := finalizeSoftware(many)
	if len(capped) != protocol.MaxSoftware {
		t.Fatalf("entry cap: got %d want %d", len(capped), protocol.MaxSoftware)
	}

	// Byte budget: big entries are cut so the serialised array stays within
	// MaxSoftwareBytes.
	big := make([]protocol.SoftwareItem, 0, 4000)
	for i := 0; i < 4000; i++ {
		big = append(big, protocol.SoftwareItem{
			Name:      fmt.Sprintf("%04d-", i) + strings.Repeat("x", 200),
			Version:   strings.Repeat("v", 100),
			Publisher: strings.Repeat("p", 200),
			Source:    protocol.SoftwareSourceDpkg,
		})
	}
	budgeted := finalizeSoftware(big)
	b, _ := json.Marshal(budgeted)
	if len(b) > protocol.MaxSoftwareBytes {
		t.Fatalf("byte budget exceeded: %d > %d", len(b), protocol.MaxSoftwareBytes)
	}
	if len(budgeted) == 0 || len(budgeted) >= 4000 {
		t.Fatalf("byte budget should have trimmed the list: %d entries", len(budgeted))
	}

	// An empty inventory returns nil so Facts.Software is omitted.
	if finalizeSoftware(nil) != nil {
		t.Fatal("empty inventory must finalise to nil")
	}
}

// softwareFixtureExec routes dpkg/rpm calls to canned output and records them,
// driven by the handler set per test.
func dpkgScan(c recordedCall) bool {
	return c.name == "dpkg-query" && len(c.args) >= 1 && c.args[0] == "-W"
}
func rpmScan(c recordedCall) bool {
	return c.name == "rpm" && len(c.args) >= 1 && c.args[0] == "-qa"
}

// TestDpkgListerPrefersDpkg checks dpkg output is used when dpkg succeeds.
func TestDpkgListerPrefersDpkg(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join("testdata", "dpkg-query.txt"))
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if dpkgScan(c) {
			return cmdResult{Stdout: string(b)}, nil
		}
		t.Fatalf("unexpected call %s %v", c.name, c.args)
		return cmdResult{}, nil
	}}
	items, err := (&dpkgLister{exec: fe}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 || items[0].Source != protocol.SoftwareSourceDpkg {
		t.Fatalf("dpkg path: %d items %+v", len(items), items)
	}
	for _, c := range fe.calls {
		if rpmScan(c) {
			t.Fatal("rpm must not be called when dpkg succeeds")
		}
	}
}

// TestDpkgListerFallsBackToRpm checks the rpm fallback when dpkg is absent, and
// the empty result when neither is present.
func TestDpkgListerFallsBackToRpm(t *testing.T) {
	rpmOut := "bash\t5.2.15-5.el9\tRocky\nopenssl\t3.0.7-18.el9_2\t(none)\n"

	// dpkg missing (start error) -> rpm used.
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if dpkgScan(c) {
			return cmdResult{ExitCode: -1}, fmt.Errorf("exec: dpkg-query: not found")
		}
		if rpmScan(c) {
			return cmdResult{Stdout: rpmOut}, nil
		}
		return cmdResult{}, nil
	}}
	items, err := (&dpkgLister{exec: fe}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Source != protocol.SoftwareSourceRPM {
		t.Fatalf("rpm fallback: %d items %+v", len(items), items)
	}

	// Neither present -> empty, no error.
	fe2 := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		return cmdResult{ExitCode: 127}, nil
	}}
	items, err = (&dpkgLister{exec: fe2}).List(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("no manager: %d items err %v", len(items), err)
	}
}

// TestSoftwareCollectorFinalizes proves Collect runs the lister and finalises
// (sorts/bounds) the result.
func TestSoftwareCollectorFinalizes(t *testing.T) {
	fe := &fakeExecutor{handler: func(c recordedCall) (cmdResult, error) {
		if dpkgScan(c) {
			return cmdResult{Stdout: "zlib\t1\tZ\ncurl\t8\tC\n"}, nil
		}
		return cmdResult{}, nil
	}}
	col := newSoftwareCollectorWith(&dpkgLister{exec: fe})
	got := col.Collect(context.Background())
	if len(got) != 2 || got[0].Name != "curl" || got[1].Name != "zlib" {
		t.Fatalf("collect: %+v", got)
	}
}

// utf8Valid reports whether s is valid UTF-8 (the cut must not split a rune).
func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
