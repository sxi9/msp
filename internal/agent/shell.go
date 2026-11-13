package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/sxi9/msp/internal/protocol"
)

// This file is the platform-neutral half of the interactive remote shell
// (RS1, decision D3, spec section C). It owns:
//
//   - the small ptySession interface the relay drives, so the copy loop is
//     platform-neutral and unit-testable with a fake PTY;
//   - Client.RunShell, the exported entrypoint the sync loop calls for a kind
//     shell command;
//   - the WebSocket dial (http->ws, https->wss, Bearer dt_ auth, reusing the
//     agent's existing TLS config and CA file through Client.http);
//   - the verbatim byte relay between the PTY and the WebSocket, resize control
//     frames, the exit frame and the agent's own idle timeout.
//
// The real PTY/ConPTY is built per platform in pty_linux.go, pty_windows.go and
// pty_other.go, each defining platformOpenPTY with the right build tag.
//
// The agent never writes shell output to its logs (spec section C): the relay
// copies bytes without logging them and keeps no transcript (the server records
// the transcript and counts the bytes).

// shellIdle is the agent's own idle timeout: with no browser->agent frame for
// this long the agent tears the session down. It is a var so tests can shorten
// it; it mirrors the server's 30-minute idle (spec section B) so the agent only
// ever acts as a backstop.
var shellIdle = 30 * time.Minute

// shellRunning is the per-agent "a shell is already running" guard (spec
// section C): a second kind shell command is refused while one is live.
var shellRunning atomic.Bool

// ptySession is the platform PTY/ConPTY the relay drives. Read yields terminal
// output (agent->browser); Write feeds keystrokes to the shell's stdin
// (browser->agent); Resize applies a window-size change; Wait blocks until the
// shell exits and returns its exit code; Close kills the shell and releases the
// PTY and must be safe to call more than once and concurrently with Wait.
type ptySession interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows uint16) error
	Wait() (int, error)
	Close() error
}

// openPTY starts the shell on a fresh PTY/ConPTY of the given initial size. It
// is a package variable so shell_test.go can substitute a fake; the real
// implementation is platformOpenPTY from the build-tagged pty_*.go files. An
// empty shell means the platform default (bash if on PATH else sh on Linux,
// powershell on Windows).
var openPTY = platformOpenPTY

// RunShell handles one kind shell command (spec section C). It is the exported
// entrypoint the sync loop calls in place of Execute for cmd.Kind == KindShell.
//
// Return contract:
//   - refused == true: the command never reached the relay (a shell is already
//     running, or the PTY could not be started on this platform). res is the
//     CommandResult the caller must POST (exit -1, error "shell busy" or the
//     platform reason), exactly like the other refuse paths.
//   - refused == false: the relay ran (or the dial failed and the server will
//     complete the command itself via the agent-never-connected timeout). res
//     is the zero value and the caller must NOT post a result: the server
//     completes a shell command when the relay closes (spec section B).
func (c *Client) RunShell(ctx context.Context, cmd protocol.Command, log *slog.Logger) (protocol.CommandResult, bool) {
	if log == nil {
		log = slog.Default()
	}
	started := time.Now().UTC()

	if !shellRunning.CompareAndSwap(false, true) {
		return protocol.CommandResult{
			ExitCode:   -1,
			StartedAt:  started,
			FinishedAt: time.Now().UTC(),
			Error:      "shell busy",
		}, true
	}
	defer shellRunning.Store(false)

	cols := uint16(protocol.ClampShellCols(cmd.Cols))
	rows := uint16(protocol.ClampShellRows(cmd.Rows))

	sess, err := openPTY(cmd.Shell, cols, rows)
	if err != nil {
		// Unsupported platform or a PTY that would not open: report it as a
		// refuse (exit -1 with the reason) so the server marks the command
		// failed with that text, as spec section C's pty_other case requires.
		return protocol.CommandResult{
			ExitCode:   -1,
			StartedAt:  started,
			FinishedAt: time.Now().UTC(),
			Error:      err.Error(),
		}, true
	}

	conn, err := c.dialShell(ctx, cmd.ID)
	if err != nil {
		// The browser is already waiting on the server; the server closes its
		// leg and completes the command "agent_never_connected" after 60 s, so
		// the agent does not post a result here (that would race the server).
		sess.Close()
		log.Warn("remote shell: dial failed", "id", cmd.ID, "err", err)
		return protocol.CommandResult{}, false
	}

	timeout := protocol.ClampShellTimeout(cmd.TimeoutS)
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	log.Info("remote shell: connected", "id", cmd.ID)
	relayShell(runCtx, conn, sess, shellIdle)
	log.Info("remote shell: closed", "id", cmd.ID)
	return protocol.CommandResult{}, false
}

// dialShell opens the agent leg of the relay: GET /api/agent/v1/shell/{id} with
// Authorization: Bearer dt_ over the same TLS config / CA file as the rest of
// the agent (Client.http), upgrading to a WebSocket. The 40 s client timeout
// bounds only the handshake: coder/websocket zeroes it on the live connection.
func (c *Client) dialShell(ctx context.Context, id int64) (*websocket.Conn, error) {
	wsURL, err := shellWSURL(c.base, id)
	if err != nil {
		return nil, err
	}
	hdr := http.Header{}
	hdr.Set("User-Agent", c.UserAgent())
	if c.token != "" {
		hdr.Set("Authorization", "Bearer "+c.token)
	}
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPClient: c.http,
		HTTPHeader: hdr,
	})
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return conn, nil
}

// shellWSURL turns the agent's normalized server base (https://host or
// http://host, already validated by NewClient) into the shell WebSocket URL,
// mapping https->wss and http->ws (spec section C).
func shellWSURL(base string, id int64) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("shell: parse server url: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
		// already a websocket scheme
	default:
		return "", fmt.Errorf("shell: unsupported server url scheme %q", u.Scheme)
	}
	u.Path = fmt.Sprintf("/api/agent/v1/shell/%d", id)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// relayShell copies frames verbatim between the WebSocket and the PTY until the
// shell exits, either leg closes, the context deadline (max duration) elapses or
// no browser->agent frame has arrived for idle (spec section B framing, section
// C agent idle):
//
//   - agent->browser: raw PTY output as binary frames;
//   - browser->agent: binary frames written to the PTY stdin;
//   - {"type":"resize"} text frames applied to the PTY (cols/rows clamped);
//   - a final {"type":"exit","code":N} text frame with the shell's exit code,
//     then a normal close.
//
// Only one goroutine writes to the WebSocket (this one), so there is no
// concurrent-writer hazard; the reader goroutine only reads.
func relayShell(ctx context.Context, conn *websocket.Conn, sess ptySession, idle time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A browser paste can exceed the 32 KiB default message limit.
	conn.SetReadLimit(1 << 20)

	// Closing the PTY when the context ends unblocks the blocking PTY Read in
	// the loop below (browser closed, idle, or max duration). Close is
	// idempotent, so the final Close at the end is harmless.
	go func() {
		<-ctx.Done()
		sess.Close()
	}()

	// Idle watchdog: reset on each browser->agent frame, fire -> tear down.
	activity := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(idle)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-activity:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(idle)
			case <-t.C:
				cancel()
				return
			}
		}
	}()

	// browser -> agent: keystrokes to PTY stdin, resize control frames applied.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			select {
			case activity <- struct{}{}:
			default:
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := sess.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				f, err := protocol.ParseControlFrame(data)
				if err != nil {
					continue // ignore anything that is not a known control frame
				}
				if f.Type == protocol.CtrlResize {
					_ = sess.Resize(
						uint16(protocol.ClampShellCols(f.Cols)),
						uint16(protocol.ClampShellRows(f.Rows)),
					)
				}
			}
		}
	}()

	// agent -> browser: PTY output as binary frames until the shell exits or a
	// leg closes.
	buf := make([]byte, 32*1024)
	for {
		n, rerr := sess.Read(buf)
		if n > 0 {
			if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	// Stop the reader and the watchdog, reap the shell, send its exit code and
	// close the socket. The exit frame uses a fresh context so a cancelled
	// runCtx (max duration / idle) does not drop it.
	cancel()
	code, _ := sess.Wait()
	writeCtx, wcancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = conn.Write(writeCtx, websocket.MessageText, protocol.ExitFrame(code).Encode())
	wcancel()
	_ = conn.Close(websocket.StatusNormalClosure, "")
	sess.Close()
	wg.Wait()
}
