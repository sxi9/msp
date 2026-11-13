// Command msp-agent is the desktop agent CLI:
//
//	msp-agent enroll  --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]
//	msp-agent run     [--config PATH] [--interval N]
//	msp-agent install --server URL --token et_... [--ca-file PEM] [--interval 30] [--insecure-http]
//	msp-agent uninstall
//	msp-agent status  [--config PATH]
//	msp-agent version
//
// Exit codes: 0 ok, 1 runtime error, 2 bad arguments or config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
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
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: msp-agent <command> [flags]

commands:
  enroll     --server URL --token et_... [--config PATH] [--ca-file PEM] [--interval 30] [--insecure-http]
  run        [--config PATH] [--interval N]
  install    --server URL --token et_... [--ca-file PEM] [--interval 30] [--insecure-http]   (root)
  uninstall                                                                                 (root)
  status     [--config PATH]
  version

default config path: %s
`, agent.DefaultPath())
}

func run(args []string, stdout, stderr io.Writer) int {
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
