package rollout

import (
	"context"
	"sync"
	"time"
)

// Keep cancels further rollout work if ownership/renewal is lost. Stop joins the
// renewer before cleanup, so a late renewal cannot race a mining resume.
func Keep(ctx context.Context, renew func(context.Context) error) (context.Context, func() error) {
	run, cancel := context.WithCancelCause(ctx)
	done, joined := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var result error
	go func() {
		defer close(joined)
		timer := time.NewTicker(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-run.Done():
				return
			case <-timer.C:
				call, end := context.WithTimeout(run, 90*time.Second)
				err := renew(call)
				end()
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return run, func() error {
		once.Do(func() {
			close(done)
			<-joined
			result = context.Cause(run)
			cancel(context.Canceled)
		})
		return result
	}
}
