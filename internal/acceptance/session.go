package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/cli"
	"github.com/lannokaede/LanAgent/internal/config"
)

// session retains evidence while making the temporary credential lifecycle local.
// Scenario functions never need to remember to remove a generated secret profile.
type session struct {
	profile, workspace, state string
}

func (v *verifier) withSession(cfg *config.Config, name, mode string, fn func(*session) (map[string]any, error)) (m map[string]any, resultErr error) {
	s := &session{profile: filepath.Join(v.work, name)}
	s.workspace, s.state = filepath.Join(s.profile, "workspace"), filepath.Join(s.profile, "state")
	defer func() {
		err := os.Remove(filepath.Join(s.profile, "config.local.yaml"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, errors.New("temporary acceptance credentials could not be removed"))
		}
	}()
	if err := s.prepare(cfg, mode, v.classifierTimeout); err != nil {
		return nil, err
	}
	return fn(s)
}

func (s *session) prepare(cfg *config.Config, mode string, classifierTimeout time.Duration) error {
	for _, dir := range []string{s.profile, s.workspace, s.state} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	values := map[string]any{
		"llm":       map[string]any{"base_url": cfg.LLM.BaseURL, "model": cfg.LLM.Model, "api_key": cfg.LLM.APIKey, "timeout": "60s", "concurrency": cfg.LLM.Concurrency, "retry": map[string]any{"max_attempts": cfg.LLM.Retry.MaxAttempts, "base_delay": cfg.LLM.Retry.BaseDelay.String()}},
		"routing":   map[string]any{"mode": mode, "timeout": classifierTimeout.String(), "classifier": map[string]any{"base_url": cfg.Routing.Classifier.BaseURL, "model": cfg.Routing.Classifier.Model, "api_key": cfg.Routing.Classifier.APIKey}},
		"rag":       map[string]any{"enabled": false},
		"practice":  map[string]any{"image": cfg.Practice.Image, "work_dir": filepath.Join(s.profile, "practice")},
		"telemetry": map[string]any{"enabled": true, "file": filepath.Join(s.profile, "spans.jsonl")},
		"context":   map[string]any{"window": 32768, "output_reserve": 4096, "tool_reserve": 2048, "summary_tokens": 512},
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(s.profile, "config.yaml"), []byte("rag:\n  enabled: false\n"), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.profile, "config.local.yaml"), raw, 0600)
}

func (s *session) run(ctx context.Context, prompt, input string, extra ...string) (string, error) {
	args := []string{"-config", s.profile, "-workspace", s.workspace, "-state-dir", s.state, "-session", "acceptance", "-timeout", "150s"}
	if prompt != "" {
		args = append(args, "-prompt", prompt)
	}
	args = append(args, extra...)
	var out, status bytes.Buffer
	runErr := cli.Run(ctx, args, strings.NewReader(input), &out, &status)
	// Keep detailed outputs private, out of the portable metadata report.
	// Append so a resumed execution retains the outage that led to recovery.
	log, logErr := os.OpenFile(filepath.Join(s.profile, "cli.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if logErr == nil {
		_, logErr = fmt.Fprintf(log, "\n--- execution ---\n%s\n%s", out.Bytes(), status.Bytes())
		logErr = errors.Join(logErr, log.Close())
	}
	if logErr != nil {
		logErr = errors.New("private acceptance CLI evidence could not be saved")
	}
	return out.String(), errors.Join(runErr, logErr)
}

func (s *session) journal() ([]map[string]any, error) {
	raw, err := os.ReadFile(filepath.Join(s.state, "acceptance.jsonl"))
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			return nil, errors.New("invalid journal")
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func metrics(rows []map[string]any) map[string]any {
	attempts, retries, known, unknown := 0, 0, 0, 0
	prompt, completion := float64(0), float64(0)
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		data, _ := e["Data"].(map[string]any)
		switch e["Type"] {
		case "model_attempt_started":
			attempts++
		case "model_retry":
			retries++
		case "model_attempt_completed":
			if usage, ok := data["usage"].(map[string]any); ok {
				known++
				p, _ := usage["prompt_tokens"].(float64)
				c, _ := usage["completion_tokens"].(float64)
				prompt += p
				completion += c
			} else {
				unknown++
			}
		}
	}
	return map[string]any{"model_attempts": attempts, "retries": retries, "usage_known_attempts": known, "usage_unknown_attempts": unknown, "reported_prompt_tokens": prompt, "reported_completion_tokens": completion}
}

func countTools(rows []map[string]any, name string) int {
	n := 0
	for _, row := range rows {
		e, _ := row["event"].(map[string]any)
		d, _ := e["Data"].(map[string]any)
		if e["Type"] == "tool_call" && d["name"] == name {
			n++
		}
	}
	return n
}
