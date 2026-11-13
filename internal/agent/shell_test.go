package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/sxi9/msp/internal/protocol"
)

// fakePTY is the test substitute for a real PTY/ConPTY: it feeds preset output
// from outCh (closing it is the shell "exit"), records keystrokes on writes and
// resizes on resizes, and reports the exit code in exit.
type fakePTY struct {
	outCh   chan []byte
	writes  chan []byte
	resizes chan [2]uint16
	exit    int
	once    sync.Once
	done    chan struct{}
}

func newFakePTY() *fakePTY {
	return &fakePTY{
		outCh:   make(chan []byte, 4),
		writes:  make(chan []byte, 8),
		resizes: make(chan [2]uint16, 8),
		done:    make(chan struct{}),
	}
}

func (f *fakePTY) Read(p []byte) (int, error) {
	select {
	case b, ok := <-f.outCh:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-f.done:
		return 0, io.EOF
	}
}

func (f *fakePTY) Write(p []byte) (int, error) {
	b := append([]byte(nil), p...)
	select {
	case f.writes <- b:
	case <-f.done:
	}
	return len(p), nil
}

func (f *fakePTY) Resize(c, r uint16) error {
	select {
	case f.resizes <- [2]uint16{c, r}:
	default:
	}
	return nil
}

func (f *fakePTY) Wait() (int, error) { return f.exit, nil }

func (f *fakePTY) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

// swapOpenPTY replaces the package openPTY hook for one test and returns a
// restore func. Tests that use it must not run in parallel (shared globals).
func swapOpenPTY(fn func(string, uint16, uint16) (ptySession, error)) func() {
	old := openPTY
	openPTY = fn
	return func() { openPTY = old }
}

func TestShellWSURL(t *testing.T) {
	cases := []struct {
		base string
		id   int64
		want string
	}{
		{"https://rmm.example.com", 7, "wss://rmm.example.com/api/agent/v1/shell/7"},
		{"http://127.0.0.1:8080", 12, "ws://127.0.0.1:8080/api/agent/v1/shell/12"},
		{"https://host:9443", 3, "wss://host:9443/api/agent/v1/shell/3"},
	}
	for _, tc := range cases {
		got, err := shellWSURL(tc.base, tc.id)
		if err != nil {
			t.Fatalf("shellWSURL(%q): %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("shellWSURL(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
	if _, err := shellWSURL("ftp://nope", 1); err == nil {
		t.Fatal("shellWSURL accepted a non-http(s) scheme")
	}
}

func TestRunShellBusy(t *testing.T) {
	shellRunning.Store(true)
	defer shellRunning.Store(false)

	c := &Client{}
	res, refused := c.RunShell(context.Background(),
		protocol.Command{ID: 1, Kind: protocol.KindShell}, nil)
	if !refused {
		t.Fatal("a second shell should be refused")
	}
	if res.ExitCode != -1 || res.Error != "shell busy" {
		t.Fatalf("unexpected busy result: %+v", res)
	}
	if res.StartedAt.IsZero() || res.FinishedAt.Before(res.StartedAt) {
		t.Fatalf("bad timestamps: %+v", res)
	}
}

func TestRunShellOpenPTYError(t *testing.T) {
	if shellRunning.Load() {
		t.Fatal("precondition: no shell should be running")
	}
	restore := swapOpenPTY(func(string, uint16, uint16) (ptySession, error) {
		return nil, errors.New("shell not supported on this platform")
	})
	defer restore()

	c := &Client{base: "http://127.0.0.1:1", http: http.DefaultClient, version: "test"}
	res, refused := c.RunShell(context.Background(),
		protocol.Command{ID: 1, Kind: protocol.KindShell}, nil)
	if !refused {
		t.Fatal("an unopenable PTY should be refused")
	}
	if res.ExitCode != -1 || res.Error != "shell not supported on this platform" {
		t.Fatalf("unexpected open-error result: %+v", res)
	}
	// The guard must be released after a refuse so the next shell can run.
	if shellRunning.Load() {
		t.Fatal("shellRunning not cleared after a refuse")
	}
}

// TestRunShellRelay drives the full agent path with a fake PTY against a real
// WebSocket: it checks the dial carries the device-token auth and the right
// path, that browser->agent keystrokes and resize frames reach the PTY, that
// PTY output reaches the browser verbatim, and that the exit frame carries the
// shell's exit code.
func TestRunShellRelay(t *testing.T) {
	if shellRunning.Load() {
		t.Fatal("precondition: no shell should be running")
	}

	fake := newFakePTY()
	fake.exit = 0
	fake.outCh <- []byte("world\n") // delivered once; EOF comes when we close it

	restore := swapOpenPTY(func(shell string, cols, rows uint16) (ptySession, error) {
		if shell != "bash" {
			t.Errorf("shell = %q, want bash", shell)
		}
		return fake, nil
	})
	defer restore()

	var (
		gotAuth  string
		gotPath  string
		output   []byte
		exitCode *int
	)
	served := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		// Act as the browser: send a keystroke and a resize.
		_ = c.Write(ctx, websocket.MessageBinary, []byte("hi"))
		_ = c.Write(ctx, websocket.MessageText, protocol.ResizeFrame(100, 40).Encode())

		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				break
			}
			if typ == websocket.MessageBinary {
				output = append(output, data...)
				continue
			}
			f, perr := protocol.ParseControlFrame(data)
			if perr == nil && f.Type == protocol.CtrlExit {
				code := -999
				if f.Code != nil {
					code = *f.Code
				}
				exitCode = &code
				break
			}
		}
		close(served)
	}))
	defer srv.Close()

	c := &Client{base: srv.URL, http: srv.Client(), token: "dt_test", version: "test"}

	runDone := make(chan struct{})
	go func() {
		res, refused := c.RunShell(context.Background(),
			protocol.Command{ID: 7, Kind: protocol.KindShell, Shell: "bash", TimeoutS: 60}, nil)
		if refused {
			t.Errorf("relay path refused unexpectedly: %+v", res)
		}
		close(runDone)
	}()

	// Keystroke must reach the PTY (proves the browser->agent reader works).
	select {
	case w := <-fake.writes:
		if string(w) != "hi" {
			t.Errorf("PTY got keystroke %q, want %q", w, "hi")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("keystroke never reached the PTY")
	}
	// Resize must reach the PTY.
	select {
	case rz := <-fake.resizes:
		if rz != [2]uint16{100, 40} {
			t.Errorf("PTY got resize %v, want [100 40]", rz)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resize never reached the PTY")
	}

	// End the shell: the next PTY read returns EOF, so the relay sends exit.
	close(fake.outCh)

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("server never saw the exit frame")
	}
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RunShell did not return")
	}

	if gotAuth != "Bearer dt_test" {
		t.Errorf("dial Authorization = %q, want %q", gotAuth, "Bearer dt_test")
	}
	if gotPath != "/api/agent/v1/shell/7" {
		t.Errorf("dial path = %q, want /api/agent/v1/shell/7", gotPath)
	}
	if string(output) != "world\n" {
		t.Errorf("relayed output = %q, want %q", output, "world\n")
	}
	if exitCode == nil || *exitCode != 0 {
		t.Errorf("exit frame = %v, want code 0", exitCode)
	}
	if shellRunning.Load() {
		t.Error("shellRunning not cleared after the relay closed")
	}
}

// TestRealPTYLinux runs a real command through the real Linux PTY and checks the
// bytes come back (spec Proof: a real PTY test guarded to linux). Skipped on
// every other platform.
func TestRealPTYLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real PTY is linux-only")
	}
	sess, err := platformOpenPTY("sh", 80, 24)
	if err != nil {
		t.Fatalf("platformOpenPTY: %v", err)
	}
	defer sess.Close()

	// Unblock the read loop if the shell somehow does not exit on its own.
	go func() {
		time.Sleep(5 * time.Second)
		sess.Close()
	}()

	if _, err := sess.Write([]byte("printf hi\nexit\n")); err != nil {
		t.Fatalf("write to pty: %v", err)
	}

	var out []byte
	buf := make([]byte, 4096)
	for {
		n, rerr := sess.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	if _, err := sess.Wait(); err != nil {
		// A shell that exits cleanly returns nil; a signal (watchdog kill)
		// returns an error, which only matters if we also missed the output.
		t.Logf("pty wait: %v", err)
	}
	if !bytes.Contains(out, []byte("hi")) {
		t.Fatalf("pty output %q does not contain %q", out, "hi")
	}
}
