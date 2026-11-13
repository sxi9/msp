// Command msp-agent is the desktop agent CLI:
//
//	msp-agent enroll  --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]
//	msp-agent run     [--config PATH] [--interval N]
//	msp-agent install --server URL --token et_... [--ca-file PEM] [--interval 30] [--insecure-http]
//	msp-agent uninstall
//	msp-agent status  [--config PATH]
//	msp-agent rustdesk setup [--password-stdin] [--config-string S] [--config PATH]   (root/Administrator)
//	msp-agent rustdesk id    [--config PATH]
//	msp-agent version
//
// Exit codes: 0 ok, 1 runtime error, 2 bad arguments or config.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sxi9/msp/internal/agent"
)

// version is set by -ldflags "-X main.version=..." (see Makefile).
var version = "dev"

const (
	exitOK      = 0
	exitRuntime = 1
	exitUsage   = 2
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: msp-agent <command> [flags]

commands:
  enroll     --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]
  run        [--config PATH] [--interval N]
  install    --server URL --token et_... [--ca-file PEM] [--interval 30] [--insecure-http]   (root)
  uninstall                                                                                 (root)
  status     [--config PATH]
  rustdesk setup [--password-stdin] [--config-string S] [--config PATH]                  (root)
             point the installed RustDesk client at the MSP's relay (host + key from the
             agent config), set its permanent password (generated, or one line piped in
             with --password-stdin; printed once and written 0600 next to the config),
             restart it and print its id
  rustdesk id [--config PATH]        print the local RustDesk id (empty when not installed)
  version

default config path: %s
`, agent.DefaultPath())
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	switch args[0] {
	case "enroll":
		return cmdEnroll(args[1:], stdout, stderr, false)
	case "install":
		return cmdEnroll(args[1:], stdout, stderr, true)
	case "run":
		return cmdRun(args[1:], stderr, log)
	case "uninstall":
		if err := agent.Uninstall(); err != nil {
			fmt.Fprintln(stderr, "uninstall:", err)
			return exitRuntime
		}
		fmt.Fprintln(stdout, "msp-agent service removed; config left at", agent.DefaultPath())
		return exitOK
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "rustdesk":
		return cmdRustdesk(args[1:], stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return exitOK
	case "help", "--help", "-h":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		usage(stderr)
		return exitUsage
	}
}

// enrollFlags are shared by enroll and install.
type enrollFlags struct {
	server, token, config, caFile string
	interval                      int
	insecureHTTP                  bool
}

func parseEnrollFlags(name string, args []string, stderr io.Writer, withConfig bool) (enrollFlags, bool) {
	var f enrollFlags
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.server, "server", "", "server base URL (https://...)")
	fs.StringVar(&f.token, "token", "", "enrollment token (et_...)")
	if withConfig {
		fs.StringVar(&f.config, "config", agent.DefaultPath(), "config file to write")
	}
	fs.StringVar(&f.caFile, "ca-file", "", "PEM file with a private CA to trust")
	fs.IntVar(&f.interval, "interval", 0, "sync interval in seconds (default 30)")
	fs.BoolVar(&f.insecureHTTP, "insecure-http", false, "allow a plain http:// server URL that is not loopback")
	if err := fs.Parse(args); err != nil {
		return f, false
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", name, fs.Arg(0))
		return f, false
	}
	if f.server == "" || f.token == "" {
		fmt.Fprintf(stderr, "%s: --server and --token are required\n", name)
		return f, false
	}
	if f.interval < 0 {
		fmt.Fprintf(stderr, "%s: --interval must be >= 0\n", name)
		return f, false
	}
	if !withConfig {
		f.config = agent.DefaultPath()
	}
	return f, true
}

// cmdEnroll enrolls and writes the config; with install=true it also
// registers and starts the service.
func cmdEnroll(args []string, stdout, stderr io.Writer, install bool) int {
	name := "enroll"
	if install {
		name = "install"
	}
	f, ok := parseEnrollFlags(name, args, stderr, !install)
	if !ok {
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := agent.Enroll(ctx, f.server, f.token, f.caFile, f.insecureHTTP, f.interval, version)
	if err != nil {
		fmt.Fprintln(stderr, name+":", err)
		if errors.Is(err, agent.ErrUnauthorized) {
			fmt.Fprintln(stderr, "the enrollment token is unknown, expired, revoked or used up; create a new one on the dashboard")
		}
		return exitRuntime
	}
	if err := agent.Save(f.config, cfg); err != nil {
		fmt.Fprintln(stderr, name+": save config:", err)
		return exitRuntime
	}
	fmt.Fprintf(stdout, "enrolled as device %d; config written to %s\n", cfg.DeviceID, f.config)
	if !install {
		return exitOK
	}
	if err := agent.Install(f.config); err != nil {
		fmt.Fprintln(stderr, "install service:", err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, "msp-agent service installed and started")
	return exitOK
}

func cmdRun(args []string, stderr io.Writer, log *slog.Logger) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", agent.DefaultPath(), "config file")
	interval := fs.Int("interval", 0, "override interval_s from the config")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "run: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	cfg, err := agent.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "run: load config:", err)
		return exitUsage
	}
	if *interval > 0 {
		cfg.IntervalS = *interval
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "run:", err)
		return exitUsage
	}
	loop := func(ctx context.Context) error {
		return agent.Run(ctx, cfg, version, log)
	}
	if agent.IsService() {
		err = agent.RunAsService(loop)
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = loop(ctx)
		stop()
	}
	if err != nil {
		fmt.Fprintln(stderr, "run:", err)
		return exitRuntime
	}
	return exitOK
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", agent.DefaultPath(), "config file")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	s, err := agent.Status(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "status:", err)
		return exitUsage
	}
	fmt.Fprint(stdout, s)
	return exitOK
}

// cmdRustdesk dispatches `msp-agent rustdesk setup|id`.
func cmdRustdesk(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "rustdesk: expected setup or id")
		return exitUsage
	}
	switch args[0] {
	case "setup":
		return cmdRustdeskSetup(args[1:], stdin, stdout, stderr)
	case "id":
		fs := flag.NewFlagSet("rustdesk id", flag.ContinueOnError)
		fs.SetOutput(stderr)
		if err := fs.Parse(args[1:]); err != nil {
			return exitUsage
		}
		id := agent.RustdeskID(context.Background())
		if id == "" {
			fmt.Fprintln(stderr, "rustdesk id: no RustDesk client found (or it has no id yet)")
			return exitRuntime
		}
		fmt.Fprintln(stdout, id)
		return exitOK
	}
	fmt.Fprintf(stderr, "rustdesk: unknown subcommand %q (expected setup or id)\n", args[0])
	return exitUsage
}

// cmdRustdeskSetup configures the installed RustDesk client for the MSP's
// relay only, sets its permanent password and prints the id. The password
// is shown here once and kept 0600 next to agent.json; it is never sent to
// the server. A chosen password comes from stdin (--password-stdin, as
// `msp-server user add` does), never from argv: the command line of a
// process is readable by every local user for as long as it runs.
func cmdRustdeskSetup(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("rustdesk setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", agent.DefaultPath(), "agent config file (holds the relay host and key)")
	passwordStdin := fs.Bool("password-stdin", false, "read the permanent password (one line, at least 8 characters) from stdin instead of generating a random 16-character one")
	configString := fs.String("config-string", "", "config string exported from a RustDesk client's Settings > Network; applied with `rustdesk --config` instead of writing RustDesk2.toml")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "rustdesk setup: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	password := ""
	if *passwordStdin {
		pw, err := readPassword(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "rustdesk setup: read password: %v\n", err)
			return exitUsage
		}
		if len(pw) < 8 {
			fmt.Fprintln(stderr, "rustdesk setup: the password on stdin must be at least 8 characters")
			return exitUsage
		}
		password = pw
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := agent.RustdeskSetup(ctx, agent.RustdeskSetupOptions{ConfigPath: *cfgPath, Password: password, ConfigString: *configString})
	if err != nil {
		fmt.Fprintln(stderr, "rustdesk setup:", err)
		if errors.Is(err, agent.ErrRustdeskNotConfigured) || errors.Is(err, agent.ErrRustdeskNotInstalled) {
			return exitUsage
		}
		return exitRuntime
	}
	fmt.Fprintf(stdout, "rustdesk client:   %s\nrelay:             %s\n", res.Binary, res.Host)
	for _, f := range res.ConfigFiles {
		fmt.Fprintf(stdout, "wrote:             %s\n", f)
	}
	if res.ID != "" {
		fmt.Fprintf(stdout, "rustdesk id:       %s\n", res.ID)
	} else {
		fmt.Fprintln(stdout, "rustdesk id:       (not readable yet; it is reported to the server on the next facts sync)")
	}
	fmt.Fprintf(stdout, "permanent password: %s\n  (shown once; kept in %s, root / SYSTEM+Administrators only; never sent to the server)\n", res.Password, res.PasswordFile)
	for _, w := range res.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	fmt.Fprintln(stdout, "the id reaches the dashboard with the agent's next facts sync (restart the msp-agent service to send it now)")
	return exitOK
}

// readPassword reads one line from r (the password for --password-stdin);
// the trailing newline is stripped and an empty line is an error.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password on stdin")
	}
	return line, nil
}
