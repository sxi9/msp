//go:build !linux && !windows

package agent

import "errors"

// platformOpenPTY is the stub for OS values that are CI cross-compile proofs
// only (android/arm64, darwin): the interactive remote shell is unsupported, so
// a kind shell command completes exit -1 with this error (spec section C,
// pty_other). Keeping the symbol here lets the agent build everywhere.
func platformOpenPTY(shell string, cols, rows uint16) (ptySession, error) {
	return nil, errors.New("shell not supported on this platform")
}
