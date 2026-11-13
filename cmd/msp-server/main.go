// Command msp-server is the MSP server CLI:
//
//	msp-server serve
//	msp-server user add --email E --role admin|operator|viewer --password-stdin
//	msp-server org add --name N                      (prints the new org id)
//	msp-server token new --org ID [--label L] [--max-uses N] [--ttl 24h]   (prints the plaintext token)
//	msp-server version
//
// Environment (flags of the same name override): MSP_DB (./msp.db), MSP_LISTEN
// (:8080), MSP_BASE_URL, MSP_TLS_CERT + MSP_TLS_KEY, MSP_SECURE_COOKIES (0/1),
// MSP_TRUST_PROXY (0/1), MSP_AGENT_DIR, MSP_RETENTION_DAYS (7).
//
// Exit codes: 0 ok, 1 runtime error, 2 bad arguments.
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sxi9/msp/internal/server"
	"github.com/sxi9/msp/internal/store"
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
	fmt.Fprint(w, `usage: msp-server <command> [flags]

commands:
  serve                                                   run the server (MSP_* env or flags)
  user add   --email E --role admin|operator|viewer --password-stdin
  org add    --name N                                     prints the new org id
  token new  --org ID [--label L] [--max-uses 10] [--ttl 168h]   prints the plaintext token
  version

env: MSP_DB MSP_LISTEN MSP_BASE_URL MSP_TLS_CERT MSP_TLS_KEY MSP_SECURE_COOKIES
     MSP_TRUST_PROXY MSP_AGENT_DIR MSP_RETENTION_DAYS
`)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr, log)
	case "user":
		if len(args) >= 2 && args[1] == "add" {
			return cmdUserAdd(args[2:], stdin, stdout, stderr)
		}
	case "org":
		if len(args) >= 2 && args[1] == "add" {
			return cmdOrgAdd(args[2:], stdout, stderr)
		}
	case "token":
		if len(args) >= 2 && args[1] == "new" {
			return cmdTokenNew(args[2:], stdout, stderr)
		}
	case "version":
		fmt.Fprintln(stdout, version)
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "msp-server: unknown command %q\n", strings.Join(args, " "))
	usage(stderr)
	return exitUsage
}

// env helpers: the flag default is the environment value, so flags override.

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envInt(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// dbFlag adds --db to a flag set, defaulting to MSP_DB or ./msp.db.
func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", envStr("MSP_DB", "./msp.db"), "SQLite database path (MSP_DB)")
}

func openStore(path string, stderr io.Writer) (*store.Store, bool) {
	st, err := store.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "msp-server: open database %s: %v\n", path, err)
		return nil, false
	}
	return st, true
}

func cmdServe(args []string, stderr io.Writer, log *slog.Logger) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := server.DefaultConfig()
	db := dbFlag(fs)
	listen := fs.String("listen", envStr("MSP_LISTEN", def.Listen), "listen address (MSP_LISTEN)")
	baseURL := fs.String("base-url", envStr("MSP_BASE_URL", ""), "public base URL for install one-liners (MSP_BASE_URL)")
	tlsCert := fs.String("tls-cert", envStr("MSP_TLS_CERT", ""), "TLS certificate PEM (MSP_TLS_CERT)")
	tlsKey := fs.String("tls-key", envStr("MSP_TLS_KEY", ""), "TLS key PEM (MSP_TLS_KEY)")
	secureCookies := fs.Bool("secure-cookies", envBool("MSP_SECURE_COOKIES"), "set Secure on the session cookie (MSP_SECURE_COOKIES)")
	trustProxy := fs.Bool("trust-proxy", envBool("MSP_TRUST_PROXY"), "honour the first X-Forwarded-For (MSP_TRUST_PROXY)")
	agentDir := fs.String("agent-dir", envStr("MSP_AGENT_DIR", ""), "directory served at /dl/ (MSP_AGENT_DIR)")
	retention := fs.Int("retention-days", envInt("MSP_RETENTION_DAYS", def.RetentionDays), "heartbeat retention in days (MSP_RETENTION_DAYS)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "msp-server serve: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		fmt.Fprintln(stderr, "msp-server serve: --tls-cert and --tls-key must be set together")
		return exitUsage
	}
	if *retention < 1 {
		fmt.Fprintln(stderr, "msp-server serve: --retention-days must be >= 1")
		return exitUsage
	}

	cfg := def
	cfg.Listen = *listen
	cfg.BaseURL = strings.TrimRight(*baseURL, "/")
	cfg.TLSCert = *tlsCert
	cfg.TLSKey = *tlsKey
	cfg.SecureCookies = *secureCookies
	cfg.TrustProxy = *trustProxy
	cfg.AgentDir = *agentDir
	cfg.RetentionDays = *retention
	cfg.Version = version

	st, ok := openStore(*db, stderr)
	if !ok {
		return exitRuntime
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := server.New(st, cfg, log)
	if err := srv.Run(ctx); err != nil {
		log.Error("server stopped", "err", err)
		return exitRuntime
	}
	log.Info("server stopped")
	return exitOK
}

func cmdUserAdd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := dbFlag(fs)
	email := fs.String("email", "", "login email")
	role := fs.String("role", "", "admin, operator or viewer")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from stdin")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	*email = strings.ToLower(strings.TrimSpace(*email))
	switch {
	case *email == "" || !strings.Contains(*email, "@"):
		fmt.Fprintln(stderr, "msp-server user add: --email must be an email address")
		return exitUsage
	case *role != "admin" && *role != "operator" && *role != "viewer":
		fmt.Fprintln(stderr, "msp-server user add: --role must be admin, operator or viewer")
		return exitUsage
	case !*passwordStdin:
		fmt.Fprintln(stderr, "msp-server user add: --password-stdin is required (pipe the password in)")
		return exitUsage
	}
	password, err := readPassword(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "msp-server user add: read password: %v\n", err)
		return exitUsage
	}
	if len(password) < 10 {
		fmt.Fprintln(stderr, "msp-server user add: password must be at least 10 characters")
		return exitUsage
	}

	st, ok := openStore(*db, stderr)
	if !ok {
		return exitRuntime
	}
	defer st.Close()
	hash, err := server.HashPassword(password)
	if err != nil {
		fmt.Fprintf(stderr, "msp-server user add: hash password: %v\n", err)
		return exitRuntime
	}
	u, err := st.CreateUser(context.Background(), *email, hash, *role)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			fmt.Fprintf(stderr, "msp-server user add: a user with email %s already exists\n", *email)
		} else {
			fmt.Fprintf(stderr, "msp-server user add: %v\n", err)
		}
		return exitRuntime
	}
	fmt.Fprintf(stdout, "created user %d %s (%s)\n", u.ID, u.Email, u.Role)
	return exitOK
}

// readPassword reads the first line of stdin (the trailing newline is not
// part of the password).
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

func cmdOrgAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("org add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := dbFlag(fs)
	name := fs.String("name", "", "organisation name")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	*name = strings.TrimSpace(*name)
	if *name == "" || len(*name) > 100 {
		fmt.Fprintln(stderr, "msp-server org add: --name is required (1..100 characters)")
		return exitUsage
	}
	st, ok := openStore(*db, stderr)
	if !ok {
		return exitRuntime
	}
	defer st.Close()
	org, err := st.CreateOrg(context.Background(), *name)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			fmt.Fprintf(stderr, "msp-server org add: an org named %q already exists\n", *name)
		} else {
			fmt.Fprintf(stderr, "msp-server org add: %v\n", err)
		}
		return exitRuntime
	}
	fmt.Fprintln(stdout, org.ID)
	return exitOK
}

func cmdTokenNew(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token new", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := dbFlag(fs)
	orgID := fs.Int64("org", 0, "organisation id")
	label := fs.String("label", "", "label shown in the dashboard")
	maxUses := fs.Int("max-uses", 10, "how many devices may enroll with it (1..1000)")
	ttl := fs.Duration("ttl", 168*time.Hour, "validity (1h..8760h)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case *orgID <= 0:
		fmt.Fprintln(stderr, "msp-server token new: --org is required")
		return exitUsage
	case *maxUses < 1 || *maxUses > 1000:
		fmt.Fprintln(stderr, "msp-server token new: --max-uses must be 1..1000")
		return exitUsage
	case *ttl < time.Hour || *ttl > 8760*time.Hour:
		fmt.Fprintln(stderr, "msp-server token new: --ttl must be between 1h and 8760h")
		return exitUsage
	case len(*label) > 100:
		fmt.Fprintln(stderr, "msp-server token new: --label must be at most 100 characters")
		return exitUsage
	}
	st, ok := openStore(*db, stderr)
	if !ok {
		return exitRuntime
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.GetOrg(ctx, *orgID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "msp-server token new: org %d does not exist\n", *orgID)
		} else {
			fmt.Fprintf(stderr, "msp-server token new: %v\n", err)
		}
		return exitRuntime
	}
	plain, _, err := st.CreateEnrollmentToken(ctx, *orgID, *label, *maxUses, *ttl, nil)
	if err != nil {
		fmt.Fprintf(stderr, "msp-server token new: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, plain)
	return exitOK
}
