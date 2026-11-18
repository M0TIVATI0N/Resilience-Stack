//go:build !chaos

// Package chaos injects deliberate faults into demo builds. Regular builds
// compile this no-op version, so production images contain no fault code.
package chaos

import (
	"context"
	"net/http"
)

// Start is a no-op in regular builds.
func Start(context.Context) {}

// Wrap returns the handler unchanged in regular builds.
func Wrap(next http.Handler) http.Handler { return next }
