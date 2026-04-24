// Package signals turns SIGTERM/SIGINT into context cancellation.
package signals

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// NewContext returns a context cancelled on SIGTERM or SIGINT. Call the
// returned CancelFunc on clean shutdown to release the handler goroutine.
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
