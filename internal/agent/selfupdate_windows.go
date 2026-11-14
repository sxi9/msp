//go:build windows

package agent

import (
	"fmt"
	"os"
)

// Windows cannot delete or overwrite the file of a running executable, but it CAN
// rename (move) it within the same directory. The service-safe swap therefore
// renames the current image aside to <exe>.old, then moves the new binary into
// place; <exe>.old stays mapped by the running process until it exits and is
// removed on the next start by cleanupSwapArtifacts (via FinalizeUpdate). This
// path is cross-built and unit-tested against the swap logic but marked NOT
// VERIFIED until exercised on a real Windows host with the service manager.

// swapExecutable renames the running image aside and moves the new binary in.
func swapExecutable(tmp, exe string) error {
	old := exe + ".old"
	os.Remove(old) // clear any leftover from a previous update
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("rename current exe aside: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		// Put the original back so the service still has a binary to restart.
		if rbErr := os.Rename(old, exe); rbErr != nil {
			return fmt.Errorf("move new exe into place: %w (and restoring the original failed: %v)", err, rbErr)
		}
		return fmt.Errorf("move new exe into place: %w", err)
	}
	return nil
}

// restoreBackup rolls back by renaming the (bad) running image aside and copying
// the backup into place. copyFile writes a fresh file, so it does not need to
// overwrite the still-mapped current image.
func restoreBackup(bak, exe string) error {
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("rename current exe aside: %w", err)
	}
	if err := copyFile(bak, exe, 0o755); err != nil {
		return fmt.Errorf("copy backup into place: %w", err)
	}
	return nil
}

// cleanupSwapArtifacts removes the renamed-aside old image left by a swap, once
// the agent has restarted and no longer maps it.
func cleanupSwapArtifacts(exe string) {
	os.Remove(exe + ".old")
}
