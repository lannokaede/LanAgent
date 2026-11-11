package personal

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/lannokaede/LanAgent/internal/llm"
)

// Statistics is reconstructed from durable metadata so attempts and reported
// usage survive model failures and process restarts, independently of graph
// checkpoints. Unknown usage is never presented as zero-cost execution.
type Statistics struct {
	Attempts      int            `json:"attempts"`
	ReportedUsage llm.Usage      `json:"reported_usage"`
	UnknownUsage  int            `json:"unknown_usage_attempts"`
	Purposes      map[string]int `json:"purposes"`
}

func (s *Session) statistics(runID string) (Statistics, error) {
	stats := Statistics{Purposes: map[string]int{}}
	f, err := s.root.Open(s.ID + ".jsonl")
	if err != nil {
		return stats, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 256<<20))
	known := 0
	for {
		var record struct {
			RunID string `json:"run_id"`
			Event struct {
				Type string
				Data llm.CallEvent
			} `json:"event"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return stats, errors.New("cannot read execution statistics")
		}
		if record.RunID != runID {
			continue
		}
		event := record.Event
		if event.Type == "model_attempt_started" {
			stats.Attempts++
			stats.Purposes[event.Data.Purpose]++
		}
		if event.Type == "model_attempt_completed" && event.Data.UsageKnown && event.Data.Usage != nil {
			known++
			stats.ReportedUsage.PromptTokens += event.Data.Usage.PromptTokens
			stats.ReportedUsage.CompletionTokens += event.Data.Usage.CompletionTokens
		}
	}
	// Detect an artificial EOF at the input cap, rather than returning partial sums.
	if info, err := f.Stat(); err != nil || info.Size() >= 256<<20 {
		return stats, errors.New("execution trace exceeds statistics limit")
	}
	stats.UnknownUsage = max(stats.Attempts-known, 0)
	return stats, nil
}
