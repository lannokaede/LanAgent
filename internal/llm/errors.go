package llm

import "fmt"

// UpstreamError exposes only the HTTP status, never credentials or provider body.
// Callers can give actionable configuration hints without leaking upstream text.
type UpstreamError struct{ StatusCode int }

func (e *UpstreamError) Error() string { return fmt.Sprintf("chat API returned HTTP %d", e.StatusCode) }
