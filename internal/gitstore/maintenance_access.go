package gitstore

import (
	"context"
	"sync"
)

// A single primary owns its Store. These admission counts are not a distributed
// filesystem lock. Unknown-scope callers conservatively protect every repo;
// scoped requests to another repository do not interrupt maintenance.
type repositoryMaintenanceAccess struct {
	mu      sync.Mutex
	all     int
	repos   map[string]int
	running *repositoryMaintenanceRun
}

type repositoryMaintenanceRun struct {
	repository string
	cancel     context.CancelFunc
	done       chan struct{}
}

// BeginMaintenanceAccess spans an entire request, including database facts
// before Git publication. Callers that cannot prove their complete repository
// scope pass no names. It is independent of snapshot capture lock ordering.
func (s *Store) BeginMaintenanceAccess(ctx context.Context, names ...string) (func(), error) {
	unique := map[string]bool{}
	for _, name := range names {
		if err := validateFullName(name); err != nil {
			return nil, err
		}
		unique[name] = true
	}
	a := &s.maintenanceAccess
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a.mu.Lock()
		run := a.running
		if run != nil && (len(unique) == 0 || run.repository == "" || unique[run.repository]) {
			run.cancel()
			done := run.done
			a.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if len(unique) == 0 {
			a.all++
		} else {
			if a.repos == nil {
				a.repos = map[string]int{}
			}
			for name := range unique {
				a.repos[name]++
			}
		}
		a.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				a.mu.Lock()
				defer a.mu.Unlock()
				if len(unique) == 0 {
					a.all--
				} else {
					for name := range unique {
						a.repos[name]--
						if a.repos[name] == 0 {
							delete(a.repos, name)
						}
					}
				}
			})
		}, nil
	}
}

// withRepositoryMaintenance never queues ahead of foreground work. It shares
// the capture barrier with mutations in OTHER repos, while snapshot capture
// interrupts it and waits for its native children to finish.
func (s *Store) withRepositoryMaintenance(ctx context.Context, name string, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a := &s.maintenanceAccess
	a.mu.Lock()
	busy := a.running != nil || a.all != 0
	if name == "" {
		busy = busy || len(a.repos) != 0
	} else {
		busy = busy || a.repos[name] != 0
	}
	if busy || !s.captureBarrier().TryAcquire(1) {
		a.mu.Unlock()
		return ErrMaintenanceBusy
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &repositoryMaintenanceRun{repository: name, cancel: cancel, done: make(chan struct{})}
	a.running = run
	a.mu.Unlock()
	defer func() {
		cancel()
		a.mu.Lock()
		s.captureBarrier().Release(1)
		a.running = nil
		close(run.done)
		a.mu.Unlock()
	}()
	return fn(runCtx)
}
