package client

import (
	"context"
	"os"
	"path/filepath"
	"sync"
)

// replace moves staged onto output. Rename replaces an existing file atomically,
// so a reader never finds the path empty; only a directory has to be cleared
// first, which cannot be done without leaving a gap.
func replace(ctx context.Context, staged string, output string) error {
	// A download abandoned at its deadline has already been reported as failed,
	// and the caller may have kept what was at output. Publishing now would take
	// that away after the fact.
	if ctx.Err() != nil {
		return contextError(ctx)
	}

	if err := os.Rename(staged, output); err == nil {
		return nil
	}

	unlock, err := outputs.lock(ctx, output)
	if err != nil {
		return err
	}
	defer unlock()

	// Another download may have replaced it while this one waited.
	if err := os.Rename(staged, output); err == nil {
		return nil
	}

	if err := os.RemoveAll(output); err != nil {
		return err
	}

	return os.Rename(staged, output)
}

// outputs holds one lock per output path, for the replacement a rename cannot do
// on its own. Naming an output after its cid makes two downloads of one cid
// collide by default rather than by accident.
//
// In-process only: it cannot see another process writing the same path.
var outputs = &pathLocks{held: make(map[string]chan struct{})}

type pathLocks struct {
	mutex sync.Mutex
	held  map[string]chan struct{}
	// waiting counts holders and waiters, so an entry is dropped once nobody is
	// using it.
	waiting map[string]int
}

// lock claims path until the returned function is called. It gives up when ctx
// does, so waiting for another download cannot outlast the caller's own deadline.
func (locks *pathLocks) lock(ctx context.Context, path string) (func(), error) {
	key := filepath.Clean(path)

	locks.mutex.Lock()
	if locks.waiting == nil {
		locks.waiting = make(map[string]int)
	}

	gate, ok := locks.held[key]
	if !ok {
		gate = make(chan struct{}, 1)
		locks.held[key] = gate
	}
	locks.waiting[key]++
	locks.mutex.Unlock()

	release := func() {
		locks.mutex.Lock()
		defer locks.mutex.Unlock()

		locks.waiting[key]--
		if locks.waiting[key] == 0 {
			delete(locks.waiting, key)
			delete(locks.held, key)
		}
	}

	select {
	case gate <- struct{}{}:
		return func() {
			<-gate
			release()
		}, nil
	case <-ctx.Done():
		release()

		return nil, contextError(ctx)
	}
}
