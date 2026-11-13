package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnitFileMatchesDeploy(t *testing.T) {
	if !strings.Contains(unitFile, "ExecStart=/usr/local/bin/msp-agent run --config /etc/msp-agent/agent.json\n") {
		t.Fatal("unit lacks the ExecStart line")
	}
	for _, want := range []string{"Restart=always", "KillMode=process", "WantedBy=multi-user.target", "After=network-online.target"} {
		if !strings.Contains(unitFile, want) {
			t.Fatalf("unit lacks %q", want)
		}
	}
	for _, forbidden := range []string{"ProtectSystem", "ProtectHome", "NoNewPrivileges"} {
		if strings.Contains(unitFile, forbidden) {
			t.Fatalf("unit must not set %s (remote admin needs root writes)", forbidden)
		}
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "msp-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != unitFile {
		t.Fatalf("deploy/msp-agent.service differs from the unitFile const:\n--- deploy\n%s\n--- const\n%s", b, unitFile)
	}
}

func TestInstallAndUninstall(t *testing.T) {
	dir := t.TempDir()
	oldBin, oldUnit, oldCtl := installBin, unitPath, systemctl
	defer func() { installBin, unitPath, systemctl = oldBin, oldUnit, oldCtl }()
	installBin = filepath.Join(dir, "bin", "msp-agent")
	unitPath = filepath.Join(dir, "systemd", "msp-agent.service")
	var calls []string
	systemctl = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}

	cfgPath := filepath.Join(dir, "agent.json")
	if err := Install(cfgPath); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(installBin)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", st.Mode())
	}
	self, _ := os.Executable()
	selfSt, _ := os.Stat(self)
	if st.Size() != selfSt.Size() {
		t.Fatalf("installed binary size %d != executable size %d", st.Size(), selfSt.Size())
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	wantExec := "ExecStart=" + installBin + " run --config " + cfgPath + "\n"
	if !strings.Contains(string(unit), wantExec) {
		t.Fatalf("unit ExecStart not rewritten for the install path:\n%s", unit)
	}
	want := []string{"daemon-reload", "enable --now msp-agent", "restart msp-agent"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}

	calls = nil
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("unit not removed: %v", err)
	}
	want = []string{"disable --now msp-agent", "daemon-reload"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}
	if _, err := os.Stat(installBin); err != nil {
		t.Fatalf("uninstall must leave the binary: %v", err)
	}
	// A second uninstall with nothing installed is still fine.
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
}

func TestRunAsServiceAndIsService(t *testing.T) {
	if IsService() {
		t.Fatal("IsService must be false on Linux")
	}
	called := false
	err := RunAsService(func(ctx context.Context) error {
		called = true
		if ctx.Err() != nil {
			t.Fatal("ctx already done")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("RunAsService: err=%v called=%v", err, called)
	}
}
