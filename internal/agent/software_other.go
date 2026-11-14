//go:build !linux && !windows

package agent

import (
	"context"

	"github.com/sxi9/msp/internal/protocol"
)

// noneSoftwareLister is the software lister for platforms with no desktop package
// inventory (the android/arm64 and darwin cross-builds). It reports nothing. The
// real Android agent (android/, Kotlin) reports apps, not software.
type noneSoftwareLister struct{}

// newPlatformSoftwareLister builds the no-op software lister.
func newPlatformSoftwareLister(commandExecutor) softwareLister { return noneSoftwareLister{} }

// List always returns an empty inventory.
func (noneSoftwareLister) List(context.Context) ([]protocol.SoftwareItem, error) { return nil, nil }
