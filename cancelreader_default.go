//go:build !darwin && !windows && !linux && !solaris && !freebsd && !netbsd && !openbsd

package ctxio

import (
	"io"
)

// NewCancelReader returns a fallbackCancelReader that satisfies the
// cancelReader but does not actually support cancelation.
func NewCancelReader(reader io.Reader) (cancelReader, error) {
	return newFallbackCancelReader(reader)
}
