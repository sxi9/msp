//go:build !windows

package agent

// restrictToPrivileged is a no-op outside Windows: the 0700 directory and
// 0600 file modes set by the callers already limit access to root.
func restrictToPrivileged(path string, dir bool) error { return nil }
