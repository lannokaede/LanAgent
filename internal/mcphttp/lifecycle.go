package mcphttp

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// Serve owns listener until shutdown. Cancellation stops admission and permits
// active HTTP requests to drain. Once the budget expires it cancels callbacks
// and closes connections, then joins HTTP and SDK work before returning. A
// successful cancellation-driven shutdown returns nil; a drain timeout remains
// discoverable through errors.Is(err, context.DeadlineExceeded).
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("HTTP transport can only serve once")
	}
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), listener.Close(), s.Close())
	}
	served := make(chan error, 1)
	go func() { served <- s.http.Serve(listener) }()
	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		return errors.Join(err, s.Close())
	case <-ctx.Done():
		s.stopAdmission()
		stop, cancel := context.WithTimeout(context.Background(), s.options.ShutdownTimeout)
		err := s.http.Shutdown(stop)
		cancel()
		closeErr := s.Close()
		serveErr := <-served
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, closeErr, serveErr)
	}
}

func (s *Server) stopAdmission() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
}

// Close is idempotent. No caller may close the tools' backing stores until it
// returns; HTTP Serve returning by itself does not establish this guarantee.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.stopAdmission()
		s.cancel()
		s.closeErr = s.http.Close()
		s.handlers.Wait()
		// Admission is closed and HTTP handlers have exited, so no new SDK
		// session can be created while iterating and closing this snapshot.
		for session := range s.mcp.Sessions() {
			s.closeErr = errors.Join(s.closeErr, session.Close())
		}
		s.tools.Wait()
	})
	return s.closeErr
}
