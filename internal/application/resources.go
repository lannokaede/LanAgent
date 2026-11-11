package application

import (
	"errors"
	"sync"
)

// resources owns successful acquisitions. Both failed Open and normal Close
// unwind the same stack, so a new dependency cannot silently miss rollback.
// Add resources in acquisition order, and the worker's stop-and-wait last.
type resources struct {
	closers []func() error
	once    sync.Once
	err     error
}

func (r *resources) add(close func() error) { r.closers = append(r.closers, close) }
func (r *resources) release(close func())   { r.add(func() error { close(); return nil }) }
func (r *resources) close() error {
	r.once.Do(func() {
		for i := len(r.closers) - 1; i >= 0; i-- {
			r.err = errors.Join(r.err, r.closers[i]())
		}
		r.closers = nil
	})
	return r.err
}
