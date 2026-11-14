//go:build !windows

package agent

import "os"

// swapExecutable replaces the running executable with the verified, health-checked
// new binary. On Linux (and other Unix) a running binary's file can be replaced
// by an atomic same-directory rename: the running process keeps its open inode,
// and the next start execs the new file. tmp and exe are in the same directory
// (the download placed tmp beside exe), so the rename is atomic.
func swapExecutable(tmp, exe string) error {
	return os.Rename(tmp, exe)
}

// restoreBackup puts the backup image back over the executable during a rollback.
// It writes through a temp file and renames into place (copyFile), which replaces
// the inode rather than truncating the mapped-in binary, so it works even while
// the agent is the running process.
func restoreBackup(bak, exe string) error {
	return copyFile(bak, exe, 0o755)
}

// cleanupSwapArtifacts removes any platform-specific swap leftovers. Unix leaves
// none (the rename consumes the temp file), so this is a no-op.
func cleanupSwapArtifacts(string) {}
