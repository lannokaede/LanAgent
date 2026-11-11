// harness-verify runs reproducible, headless acceptance checks. Live checks use
// isolated workspaces, databases and uniquely named collections, never user data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

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
type verifier struct {
	report                             report
	root, work, config, milvus, corpus string
	classifierTimeout                  time.Duration
	only                               string
}

func (v *verifier) run(name, mode string, fn func() (map[string]any, error)) {
	if v.only != "" && name != v.only {
		return
	}
	start := time.Now()
	metrics, err := fn()
	row := check{Name: name, Mode: mode, Status: "passed", DurationMS: float64(time.Since(start)) / float64(time.Millisecond), Metrics: metrics}
	if err != nil {
		row.Status = "failed"
		row.Error = err.Error()
		v.report.Success = false
	}
	v.report.Checks = append(v.report.Checks, row)
	fmt.Printf("%s: %s\n", name, row.Status)
	if err != nil {
		fmt.Println("  " + row.Error)
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("harness-verify", flag.ContinueOnError)
	mode := f.String("mode", "offline", "offline, live or all")
	only := f.String("only", "", "run one named check")
	cfg := f.String("config", "configs/interview", "existing API/embedding configuration, read only")
	output := f.String("report", ".runtime/harness-validation/report.json", "JSON report path")
	milvus := f.String("milvus-url", "", "override Milvus for isolated live collections only")
	corpus := f.String("corpus-dir", "", "optional existing corpus snapshot to verify counts and checksums")
	classifierTimeout := f.Duration("classifier-timeout", 15*time.Second, "live classification budget, recorded in report")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || (*mode != "offline" && *mode != "live" && *mode != "all") || *classifierTimeout <= 0 || *classifierTimeout > 30*time.Second {
		return errors.New("invalid verification arguments")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return errors.New("run inside the LanAgent repository")
		}
		root = parent
	}
	base := filepath.Join(root, ".runtime", "harness-validation")
	if err = os.MkdirAll(base, 0700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return err
	}
	// Runtime evidence is deliberately retained for inspection; all credentials
	// live in private temporary profiles, removed before writing the final report.
	v := &verifier{only: *only, root: root, work: work, config: *cfg, milvus: *milvus, corpus: *corpus, classifierTimeout: *classifierTimeout, report: report{Version: 1, StartedAt: time.Now().UTC(), GoVersion: runtime.Version(), Mode: *mode, Success: true}}
	if *mode == "offline" || *mode == "all" {
		v.offline(ctx)
	}
	if *mode == "live" || *mode == "all" {
		v.live(ctx)
	}
	if *corpus != "" {
		v.run("corpus_snapshot", "offline", func() (map[string]any, error) { return verifyCorpus(ctx, *corpus) })
	}
	if len(v.report.Checks) == 0 {
		return errors.New("no checks matched -only")
	}
	path := *output
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v.report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "# Harness acceptance report\n\nMode: `%s` · Go: `%s` · Started: `%s`\n\n| Check | Mode | Status | Duration (ms) |\n| --- | --- | --- | ---: |\n", v.report.Mode, v.report.GoVersion, v.report.StartedAt.Format(time.RFC3339))
	for _, c := range v.report.Checks {
		fmt.Fprintf(&md, "| %s | %s | %s | %.1f |\n", c.Name, c.Mode, c.Status, c.DurationMS)
	}
	md.WriteString("\nSee the companion JSON for metrics and failures. Unknown model usage is not zero usage. Offline fixture results do not establish real-model accuracy or production throughput.\n")
	if err = os.WriteFile(strings.TrimSuffix(path, filepath.Ext(path))+".md", []byte(md.String()), 0600); err != nil {
		return err
	}
	fmt.Println("Report:", path)
	if !v.report.Success {
		return errors.New("acceptance checks failed; see report")
	}
	return nil
}
func (v *verifier) offline(ctx context.Context) {
	v.run("offline_regression", "offline", func() (map[string]any, error) {
		packages := []string{"./internal/harness", "./internal/contextbudget", "./internal/llm", "./internal/personal", "./internal/personalmemory", "./internal/extension", "./internal/cli", "./internal/interview", "./cmd/harness"}
		args := append([]string{"test", "-json", "-count=1"}, packages...)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = v.root
		cmd.Env = append(os.Environ(), "LANAGENT_API_CONFIG="+filepath.Join(v.work, "nonexistent-api.json"))
		stdout, err := cmd.Output()
		passed, failed, skipped := 0, 0, 0
		for _, line := range strings.Split(string(stdout), "\n") {
			var event struct{ Action, Test string }
			if json.Unmarshal([]byte(line), &event) != nil || event.Test == "" {
				continue
			}
			switch event.Action {
			case "pass":
				passed++
			case "fail":
				failed++
			case "skip":
				skipped++
			}
		}
		metrics := map[string]any{"passed_tests": passed, "failed_tests": failed, "skipped_tests": skipped, "packages": len(packages)}
		if err != nil {
			os.WriteFile(filepath.Join(v.work, "offline.log"), stdout, 0600)
			return metrics, errors.New("offline regression failed; inspect private runtime log")
		}
		return metrics, nil
	})
}
