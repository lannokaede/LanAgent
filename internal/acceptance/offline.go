package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (v *verifier) offline(ctx context.Context) {
	v.run(ctx, "offline_regression", "offline", func() (map[string]any, error) {
		packages := []string{
			"./internal/acceptance", "./internal/application", "./internal/architecture",
			"./internal/harness", "./internal/contextbudget", "./internal/llm", "./internal/rag",
			"./internal/personal", "./internal/personalmemory", "./internal/extension", "./internal/cli",
			"./internal/interview", "./cmd/harness", "./cmd/harness-verify",
		}
		args := append([]string{"test", "-json", "-count=1"}, packages...)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = v.root
		cmd.Env = append(os.Environ(), "LANAGENT_API_CONFIG="+filepath.Join(v.work, "nonexistent-api.json"))
		stdout, err := cmd.CombinedOutput()
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
			failure := errors.New("offline regression failed; inspect private runtime log")
			if err := os.WriteFile(filepath.Join(v.work, "offline.log"), stdout, 0600); err != nil {
				failure = errors.Join(failure, errors.New("private offline evidence could not be saved"))
			}
			return metrics, failure
		}
		return metrics, nil
	})
}
