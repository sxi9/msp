package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sxi9/msp/internal/protocol"
)

// Remote desktop (decision D4 phase A, feature RD1): the agent does not ship
// or build RustDesk. It talks to an already installed official RustDesk
// client through that client's documented command line, which is what the
// official client-deployment page uses for unattended installs:
//
//	rustdesk --get-id            prints the device id            (facts.rustdesk_id)
//	rustdesk --password <pw>     sets the permanent password     (rustdesk setup)
//	rustdesk --config <string>   applies an exported config string (rustdesk setup --config-string)
//
// Pointing the client at OUR relay is done by writing the [options] block of
// RustDesk2.toml (custom-rendezvous-server, relay-server, key). Of those
// three keys only custom-rendezvous-server is verbatim-confirmed from the
// RustDesk source (src/core_main.rs); relay-server and key are the
// conventional names, and the per-OS locations of RustDesk2.toml below are
// the conventional ones, not quoted from primary documentation. They are
// therefore a manual-test item (docs/STATUS.md). When the MSP prefers the
// fully documented path, the config string exported from the client's
// Settings > Network can be passed with --config-string and is applied with
// `rustdesk --config` instead of the TOML write.
//
// Everything below is overridable for tests: the binary, the config
// directories, the restart hook and the timeout.

// ErrRustdeskNotInstalled is returned by RustdeskSetup when no RustDesk
// client binary can be found on this host.
var ErrRustdeskNotInstalled = errors.New("rustdesk client not installed (run the installer with --with-rustdesk / -WithRustDesk, or install the official client and retry)")

// ErrRustdeskNotConfigured is returned when the agent config holds no relay:
// the server has MSP_RUSTDESK_HOST/KEY unset, or the agent never synced
// since they were set.
var ErrRustdeskNotConfigured = errors.New("no RustDesk relay in the agent config: set MSP_RUSTDESK_HOST and MSP_RUSTDESK_KEY on the server and let the agent sync once (or re-enroll)")

var (
	// rustdeskBin overrides binary discovery when non-empty (tests, or an
	// unusual install location via MSP_RUSTDESK_BIN).
	rustdeskBin = os.Getenv("MSP_RUSTDESK_BIN")
	// rustdeskTimeout bounds every call into the client.
	rustdeskTimeout = 20 * time.Second
	// rustdeskConfigDirs lists the directories whose RustDesk2.toml is
	// written by setup and whose RustDesk.toml is read as the id fallback.
	rustdeskConfigDirs = defaultRustdeskConfigDirs
	// rustdeskRestart restarts the RustDesk service after a config change
	// (best effort: a failure is reported, not fatal).
	rustdeskRestart = defaultRustdeskRestart
	// rustdeskPasswordLen is the length of a generated permanent password.
	rustdeskPasswordLen = 16
)

// rustdeskBinary returns the path of the RustDesk client, "" when it is not
// installed. Linux: `rustdesk` on PATH or /usr/bin/rustdesk (the .deb puts it
// there). Windows: %ProgramFiles%\RustDesk\rustdesk.exe (the default of the
// official installer) or rustdesk.exe on PATH.
func rustdeskBinary() string {
	if rustdeskBin != "" {
		if _, err := os.Stat(rustdeskBin); err == nil {
			return rustdeskBin
		}
		return ""
	}
	var candidates []string
	if runtime.GOOS == "windows" {
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		candidates = []string{filepath.Join(pf, "RustDesk", "rustdesk.exe")}
		if p, err := exec.LookPath("rustdesk.exe"); err == nil {
			candidates = append(candidates, p)
		}
	} else {
		if p, err := exec.LookPath("rustdesk"); err == nil {
			candidates = append(candidates, p)
		}
		candidates = append(candidates, "/usr/bin/rustdesk")
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// defaultRustdeskConfigDirs are the conventional RustDesk config directories
// of the account the RustDesk service runs as (root on Linux; LocalService
// on Windows) plus, on Windows, the invoking user's own. Unverified from
// primary docs: see the file comment.
func defaultRustdeskConfigDirs() []string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		dirs := []string{filepath.Join(root, "ServiceProfiles", "LocalService", "AppData", "Roaming", "RustDesk", "config")}
		if app := os.Getenv("APPDATA"); app != "" {
			dirs = append(dirs, filepath.Join(app, "RustDesk", "config"))
		}
		return dirs
	}
	return []string{"/root/.config/rustdesk"}
}

// defaultRustdeskRestart restarts the client service: `systemctl restart
// rustdesk` on Linux (the official deployment script's step after --config
// and --password); on Windows the "RustDesk" service through net.exe.
func defaultRustdeskRestart(ctx context.Context) error {
	var steps [][]string
	switch runtime.GOOS {
	case "linux":
		steps = [][]string{{"systemctl", "restart", "rustdesk"}}
	case "windows":
		steps = [][]string{{"net", "stop", "RustDesk"}, {"net", "start", "RustDesk"}}
	default:
		return nil
	}
	for _, argv := range steps {
		if _, err := rustdeskExec(ctx, argv[0], argv[1:]...); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
		}
	}
	return nil
}

// rustdeskExec runs one command with the RustDesk timeout and returns its
// combined output; a non-zero exit is an error carrying the output.
func rustdeskExec(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, rustdeskTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin = nil
	out, err := c.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return text, fmt.Errorf("timeout after %s", rustdeskTimeout)
		}
		if text != "" {
			return text, fmt.Errorf("%w: %s", err, firstLineOf(text))
		}
		return text, err
	}
	return text, nil
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// rustdeskID returns the id of the installed RustDesk client, "" when it is
// not installed or the id cannot be read. It asks `rustdesk --get-id` (the
// documented way) and falls back to the id line of RustDesk.toml in the
// conventional config directories. Only a value that passes
// protocol.ValidRustdeskID is ever reported.
func rustdeskID(ctx context.Context) string {
	if bin := rustdeskBinary(); bin != "" {
		if out, err := rustdeskExec(ctx, bin, "--get-id"); err == nil {
			if id := lastDigitsLine(out); id != "" {
				return id
			}
		}
	}
	for _, dir := range rustdeskConfigDirs() {
		if id := rustdeskIDFromTOML(filepath.Join(dir, "RustDesk.toml")); id != "" {
			return id
		}
	}
	return ""
}

// lastDigitsLine returns the last non-empty line of out when it is a valid
// RustDesk id (the client may print log lines before the id).
func lastDigitsLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if protocol.ValidRustdeskID(l) {
			return l
		}
		return ""
	}
	return ""
}

// rustdeskIDFromTOML reads `id = '123456789'` (single or double quotes) from
// a RustDesk.toml file; "" when absent or invalid.
func rustdeskIDFromTOML(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "id") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "id" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `'"`)
		if protocol.ValidRustdeskID(val) {
			return val
		}
		return ""
	}
	return ""
}

// rustdeskOptions are the [options] keys that point a client at a relay.
// custom-rendezvous-server is the ID server (hbbs, port 21116); relay-server
// the relay (hbbr, port 21117): the same bare host, each on its default
// port, which is why the host carries no port (protocol.ValidRustdeskHost);
// key the server's public key. api-server is left alone. The values are
// emitted into TOML literal strings without escaping, so callers validate
// them first (RustdeskSetup) and the validator admits no quote, control or
// whitespace character.
func rustdeskOptions(host, key string) [][2]string {
	return [][2]string{
		{"custom-rendezvous-server", host},
		{"relay-server", host},
		{"key", key},
	}
}

// mergeRustdeskOptions returns existing (the current RustDesk2.toml, "" when
// absent) with the given keys set inside its [options] table: lines for
// those keys are replaced wherever they appear in [options], the other
// lines and tables are kept, and a missing [options] table is appended. The
// result always ends with a newline.
func mergeRustdeskOptions(existing string, opts [][2]string) string {
	set := map[string]string{}
	for _, kv := range opts {
		set[kv[0]] = kv[1]
	}
	var out []string
	inOptions := false
	seenOptions := false
	emit := func() {
		for _, kv := range opts {
			out = append(out, fmt.Sprintf("%s = '%s'", kv[0], kv[1]))
		}
	}
	lines := strings.Split(strings.ReplaceAll(existing, "\r\n", "\n"), "\n")
	if existing == "" {
		lines = nil
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			if inOptions {
				emit()
			}
			inOptions = t == "[options]"
			if inOptions {
				seenOptions = true
			}
			out = append(out, line)
			continue
		}
		if inOptions {
			if k, _, ok := strings.Cut(t, "="); ok {
				if _, replace := set[strings.TrimSpace(k)]; replace {
					continue
				}
			}
		}
		out = append(out, line)
	}
	if inOptions {
		emit()
	}
	if !seenOptions {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "[options]")
		emit()
	}
	// Drop trailing empty lines, then end with exactly one newline.
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n") + "\n"
}

// writeRustdeskOptions writes the [options] relay keys into dir/RustDesk2.toml
// (dir created 0700, file 0600) and returns the file path.
func writeRustdeskOptions(dir, host, key string) (string, error) {
	path := filepath.Join(dir, "RustDesk2.toml")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return path, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, err
	}
	merged := mergeRustdeskOptions(string(existing), rustdeskOptions(host, key))
	if err := os.WriteFile(path, []byte(merged), 0o600); err != nil {
		return path, err
	}
	return path, nil
}

// passwordAlphabet avoids characters that are easily confused when read off
// a screen (0/O, 1/l/I) and anything a shell would quote.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// newRustdeskPassword returns a random permanent password of
// rustdeskPasswordLen characters.
func newRustdeskPassword() (string, error) {
	b := make([]byte, rustdeskPasswordLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = passwordAlphabet[int(b[i])%len(passwordAlphabet)]
	}
	return string(b), nil
}

// RustdeskSetupOptions parameterises RustdeskSetup.
type RustdeskSetupOptions struct {
	ConfigPath   string // agent.json (DefaultPath when empty)
	Password     string // permanent password; generated when empty
	ConfigString string // optional: exported RustDesk config string, applied with `rustdesk --config` instead of the TOML write
}

// RustdeskSetupResult reports what RustdeskSetup did.
type RustdeskSetupResult struct {
	Binary       string
	Host         string
	ID           string   // "" when --get-id gave nothing usable
	Password     string   // the permanent password now set
	PasswordFile string   // where it was written (0600, next to agent.json)
	ConfigFiles  []string // RustDesk2.toml files written (empty with ConfigString)
	Warnings     []string // non-fatal problems (restart failed, id unreadable, ...)
}

// RustdeskSetup configures the installed RustDesk client for the MSP's own
// relay only, sets its permanent password, restarts it and reads its id:
//
//  1. load agent.json; the relay host and key must be there (ErrRustdeskNotConfigured);
//  2. find the client (ErrRustdeskNotInstalled);
//  3. apply the relay: `rustdesk --config <string>` when ConfigString is set,
//     else write [options] into RustDesk2.toml in every config dir;
//  4. `rustdesk --password <pw>` (pw generated when not given);
//  5. restart the service (best effort);
//  6. `rustdesk --get-id`;
//  7. write the password 0600 to <agent config dir>/rustdesk-password
//     (on Windows with a SYSTEM + Administrators DACL).
//
// The password is never sent to the MSP server; it is printed by the CLI
// once and left in that file for a technician on the box. Step 4 puts it
// on the rustdesk process command line for the duration of that call; that
// is the only documented way to set it (docs/SECURITY.md section 8).
func RustdeskSetup(ctx context.Context, o RustdeskSetupOptions) (RustdeskSetupResult, error) {
	var res RustdeskSetupResult
	cfgPath := o.ConfigPath
	if cfgPath == "" {
		cfgPath = DefaultPath()
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		return res, fmt.Errorf("load config: %w", err)
	}
	if cfg.RustdeskHost == "" || cfg.RustdeskKey == "" {
		return res, ErrRustdeskNotConfigured
	}
	// Re-checked here, not only when the server sent it: the values go
	// verbatim into RustDesk2.toml and an edited agent.json must not be
	// able to inject further [options] keys or break the file.
	if err := (protocol.Rustdesk{Host: cfg.RustdeskHost, Key: cfg.RustdeskKey}).Validate(); err != nil {
		return res, fmt.Errorf("relay in %s is unusable: %w", cfgPath, err)
	}
	res.Host = cfg.RustdeskHost
	res.Binary = rustdeskBinary()
	if res.Binary == "" {
		return res, ErrRustdeskNotInstalled
	}

	if o.ConfigString != "" {
		if out, err := rustdeskExec(ctx, res.Binary, "--config", o.ConfigString); err != nil {
			return res, fmt.Errorf("rustdesk --config: %w", err)
		} else if out != "" {
			res.Warnings = append(res.Warnings, "rustdesk --config said: "+firstLineOf(out))
		}
	} else {
		dirs := rustdeskConfigDirs()
		if len(dirs) == 0 {
			return res, errors.New("no RustDesk config directory for this platform")
		}
		for _, dir := range dirs {
			path, err := writeRustdeskOptions(dir, cfg.RustdeskHost, cfg.RustdeskKey)
			if err != nil {
				return res, fmt.Errorf("write %s: %w", path, err)
			}
			res.ConfigFiles = append(res.ConfigFiles, path)
		}
	}

	pw := o.Password
	if pw == "" {
		if pw, err = newRustdeskPassword(); err != nil {
			return res, fmt.Errorf("generate password: %w", err)
		}
	}
	if _, err := rustdeskExec(ctx, res.Binary, "--password", pw); err != nil {
		return res, fmt.Errorf("rustdesk --password: %w", err)
	}
	res.Password = pw

	if err := rustdeskRestart(ctx); err != nil {
		res.Warnings = append(res.Warnings, "restart of the RustDesk service failed, restart it (or reboot) before connecting: "+err.Error())
	}

	if out, err := rustdeskExec(ctx, res.Binary, "--get-id"); err != nil {
		res.Warnings = append(res.Warnings, "rustdesk --get-id failed: "+err.Error())
	} else if res.ID = lastDigitsLine(out); res.ID == "" {
		res.Warnings = append(res.Warnings, "rustdesk --get-id printed no id: "+firstLineOf(out))
	}

	// 0700/0600 on Linux; on Windows the mode bits mean nothing, so the
	// directory and the file get a SYSTEM + Administrators DACL (see
	// restrictToPrivileged), the same one install.ps1 applies with icacls,
	// so a manual `msp-agent install` + `rustdesk setup` is covered too.
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	if err := restrictToPrivileged(dir, true); err != nil {
		return res, err
	}
	res.PasswordFile = filepath.Join(dir, "rustdesk-password")
	if err := os.WriteFile(res.PasswordFile, []byte(pw+"\n"), 0o600); err != nil {
		return res, fmt.Errorf("write %s: %w", res.PasswordFile, err)
	}
	if err := os.Chmod(res.PasswordFile, 0o600); err != nil {
		return res, err
	}
	if err := restrictToPrivileged(res.PasswordFile, false); err != nil {
		return res, err
	}
	return res, nil
}

// RustdeskID is the exported form of rustdeskID for the CLI.
func RustdeskID(ctx context.Context) string { return rustdeskID(ctx) }
