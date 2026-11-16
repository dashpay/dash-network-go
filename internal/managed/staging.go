package managed

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/dashpay/dash-network-go/internal/node"
)

// Match native upgrade staging: image pulls are independent per host and never
// withdraw a service. Container replacement still runs one target at a time.
const stagingConcurrency = 16

// stage waits for every started call before returning, including on failure or
// cancellation, so the runner cannot release its claim with staging in flight.
// Workers only read the reviewed plan; progress and journal writes stay on the
// calling goroutine. All host failures are retained, not just the first reply.
func (r Runner) stage(ctx context.Context, p Plan) error {
	type result struct {
		name string
		err  error
	}
	jobs := make(chan Request, len(p.Snapshot.Fleet.Targets))
	for _, t := range p.Snapshot.Fleet.Targets {
		if len(p.Images[t.Name]) == 0 {
			continue
		}
		q := request(p.Snapshot.Fleet, t, "stage")
		v := p.Snapshot.Nodes[t.Name]
		q.Expected = &v
		q.Pins = p.Images[t.Name]
		q.Operation = p.Operation
		q.OperationID = p.ID
		q.Migration = p.Migration
		jobs <- q
	}
	close(jobs)
	results := make(chan result)
	var wg sync.WaitGroup
	for range min(stagingConcurrency, len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range jobs {
				err := ctx.Err()
				if err == nil {
					_, err = r.Remote.Call(ctx, q)
				}
				results <- result{q.Target.Name, err}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	var failures []string
	for x := range results {
		if x.err != nil {
			failures = append(failures, x.name+": "+node.SafeError(x.err))
		} else {
			r.report("managed target " + x.name + " staged")
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return errors.Join(errors.New("image staging failed: "+strings.Join(failures, "; ")), ctx.Err())
	}
	return ctx.Err()
}
