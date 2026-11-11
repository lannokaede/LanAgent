package llm

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UpstreamError exposes only the HTTP status, never credentials or provider body.
// Callers can give actionable configuration hints without leaking upstream text.
type UpstreamError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("chat API returned HTTP %d", e.StatusCode) }

func upstreamError(status int, header http.Header) *UpstreamError {
	e := &UpstreamError{StatusCode: status}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		// Bound arithmetic, not the server-requested delay. A delay beyond the call
		// deadline is never shortened to retry earlier than the server permits.
		if seconds > int64(time.Duration(1<<63-1)/time.Second) {
			e.RetryAfter = time.Duration(1<<63 - 1)
		} else {
			e.RetryAfter = time.Duration(seconds) * time.Second
		}
	} else if date, err := http.ParseTime(raw); err == nil {
		e.RetryAfter = max(time.Until(date), 0)
	}
	return e
}
