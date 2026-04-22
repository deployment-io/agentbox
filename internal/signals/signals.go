// Package signals installs OS signal handlers that cancel a context on
// SIGTERM / SIGINT. The caller's subprocess should watch the returned
// context and shut down cleanly.
package signals

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// NewContext returns a context that is cancelled when SIGTERM or SIGINT
// is received. The returned cancel function should be called on clean
// shutdown to release the signal handler goroutine.
func NewContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		defer signal.Stop(ch)
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()

	return ctx, cancel
}
