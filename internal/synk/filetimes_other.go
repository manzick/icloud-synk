//go:build !linux

package synk

import (
	"os"
	"time"
)

// Preserve the actual write timestamp where descriptor-based timestamp updates
// are unavailable. Reconciliation records the observed timestamp either way.
func setFileTimes(_ *os.File, _ time.Time) error { return nil }
