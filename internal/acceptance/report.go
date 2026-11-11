package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The versioned report is deliberately metadata-only. Model output, tool output,
// and credentials stay in the private runtime evidence instead of this format.
type check struct {
	Name       string         `json:"name"`
	Mode       string         `json:"mode"`
	Status     string         `json:"status"`
	DurationMS float64        `json:"duration_ms"`
	Metrics    map[string]any `json:"metrics,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type report struct {
	Version   int       `json:"version"`
	StartedAt time.Time `json:"started_at"`
	GoVersion string    `json:"go_version"`
	Mode      string    `json:"mode"`
	Success   bool      `json:"success"`
	Checks    []check   `json:"checks"`
}

func (v *verifier) writeReport(path string) error {
	data, err := json.MarshalIndent(v.report, "", "  ")
	if err != nil {
		return err
	}
	if err := writeEvidence(path, append(data, '\n')); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# Harness acceptance report\n\nMode: `%s` · Go: `%s` · Started: `%s`\n\n| Check | Mode | Status | Duration (ms) |\n| --- | --- | --- | ---: |\n", v.report.Mode, v.report.GoVersion, v.report.StartedAt.Format(time.RFC3339))
	for _, c := range v.report.Checks {
		fmt.Fprintf(&md, "| %s | %s | %s | %.1f |\n", c.Name, c.Mode, c.Status, c.DurationMS)
	}
	md.WriteString("\nSee the companion JSON for metrics and failures. Unknown model usage is not zero usage. Offline fixture results do not establish real-model accuracy or production throughput.\n")
	markdownPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".md"
	if markdownPath == path {
		markdownPath += ".md"
	}
	return writeEvidence(markdownPath, []byte(md.String()))
}

// Rename prevents a canceled or failed write from exposing a truncated report.
func writeEvidence(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".report-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
