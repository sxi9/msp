package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeAptGet is a stub apt-get for the e2e tests (PM1). A simulated
// dist-upgrade prints a canned pending list (two security updates, one regular);
// any install prints a success log. It never touches real packages. The scan
// matches "dist-upgrade" first because that string also contains "upgrade".
const fakeAptGet = `#!/bin/sh
case "$*" in
  *dist-upgrade*)
    echo "Reading package lists..."
    echo "Building dependency tree..."
    echo "Calculating upgrade..."
    echo "Inst libssl3 [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:24.04/noble-security [amd64])"
    echo "Inst openssl [3.0.2-0ubuntu1.18] (3.0.2-0ubuntu1.19 Ubuntu:24.04/noble-security [amd64])"
    echo "Inst curl [8.5.0-2ubuntu10.1] (8.5.0-2ubuntu10.6 Ubuntu:24.04/noble-updates [amd64])"
    exit 0
    ;;
  *install*|*upgrade*)
    echo "Reading package lists..."
    echo "fake apt-get: upgraded the requested packages"
    exit 0
    ;;
esac
exit 0
`

// TestMain prepends a fake apt-get to PATH so the in-process agent's patch path
// (PM1) never runs the real apt in any e2e test: only apt-get is shadowed, every
// other binary still resolves from the real PATH. The whole package runs its
// tests serially (none call t.Parallel), so one process-wide PATH is safe.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "msp-e2e-fakeapt-")
	if err != nil {
		panic("e2e: temp dir for fake apt-get: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(dir, "apt-get"), []byte(fakeAptGet), 0o755); err != nil {
		panic("e2e: write fake apt-get: " + err.Error())
	}
	os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
