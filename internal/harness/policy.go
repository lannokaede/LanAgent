package harness

import (
	"errors"
	"fmt"
	"time"
)

var ErrToolBudget = errors.New("harness tool call budget exhausted")

// ToolPolicy applies to one Run, across all model rounds. Zero disables a limit
// for legacy applications. Timeout is cooperative: tools must honor context.
// Configure the runner before sharing it between goroutines.
type ToolPolicy struct {
	MaxCalls       int
	MaxArgsBytes   int
	MaxResultBytes int
	Timeout        time.Duration
}

func (p ToolPolicy) validate() error {
	if p.MaxCalls < 0 || p.MaxArgsBytes < 0 || p.MaxResultBytes < 0 || p.Timeout < 0 {
		return fmt.Errorf("harness tool limits must not be negative")
	}
	return nil
}
