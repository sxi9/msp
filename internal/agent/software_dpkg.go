//go:build linux

package agent

import (
	"context"

	"github.com/sxi9/msp/internal/protocol"
)

// dpkgLister reads installed software on Linux (SI1). It tries dpkg first (the
// Debian/Ubuntu target) and falls back to rpm on an RPM distro; every call goes
// through the injected commandExecutor so tests feed canned output and no test
// touches the real package database. Both commands are read-only queries.
type dpkgLister struct {
	exec commandExecutor
}

// newPlatformSoftwareLister builds the Linux (dpkg/rpm) software lister.
func newPlatformSoftwareLister(x commandExecutor) softwareLister { return &dpkgLister{exec: x} }

// dpkgQueryFormat and rpmQueryFormat are the one-line-per-package output formats
// the parsers expect (name, version, maintainer/vendor, tab-separated).
const (
	dpkgQueryFormat = "${Package}\t${Version}\t${Maintainer}\n"
	rpmQueryFormat  = "%{NAME}\t%{VERSION}-%{RELEASE}\t%{VENDOR}\n"
)

// List queries dpkg, then rpm, then gives up with an empty inventory (not an
// error). A non-zero exit or a start failure for one manager falls through to
// the next; neither present yields nil.
func (l *dpkgLister) List(ctx context.Context) ([]protocol.SoftwareItem, error) {
	if res, err := l.exec.run(ctx, nil, "dpkg-query", "-W", "-f", dpkgQueryFormat); err == nil && res.ExitCode == 0 {
		return parseDpkg(res.Stdout), nil
	}
	if res, err := l.exec.run(ctx, nil, "rpm", "-qa", "--qf", rpmQueryFormat); err == nil && res.ExitCode == 0 {
		return parseRpm(res.Stdout), nil
	}
	return nil, nil
}
