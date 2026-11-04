package practice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CLI contract tests do not claim real container isolation; live suite is separate.
func TestDockerFixedArgumentsAndCleanup(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	t.Setenv("PRACTICE_ARGV_FILE", record)
	executable := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PRACTICE_ARGV_FILE"
case "$1" in
 version|image|ps|create|rm) exit 0;;
 start) printf '{"Action":"pass","Package":"exercise"}\n';exit 0;;
 inspect) printf '{"Running":false,"ExitCode":0,"OOMKilled":false}\n';exit 0;;
 *) exit 1;;
esac
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e := catalog(t).List()[0]
	runner := NewDockerExecutor("trusted-image", "test", filepath.Join(dir, "snapshots"))
	runner.Binary = executable
	result := runner.Execute(context.Background(), e, fixture(t, e, "correct"), "test-run")
	if result.Status != "passed" {
		t.Fatalf("%#v", result)
	}
	b, _ := os.ReadFile(record)
	args := string(b)
	for _, flag := range []string{"--network none", "--user 10001:10001", "--read-only", "--cap-drop ALL", "no-new-privileges", "--memory 1g", "--cpus 1", "--pids-limit 128", "--log-driver none", "/tmp:rw,exec,nosuid,size=768m,mode=1777", "dst=/workspace,readonly", "go test -json -race -count=1 -timeout=10s ./...", "rm -f lanagent-practice-"} {
		if !strings.Contains(args, flag) {
			t.Fatal("missing isolation argument", flag, args)
		}
	}
	if strings.Contains(args, "func ") || strings.Contains(args, "sh -c") || strings.Contains(args, "docker.sock") {
		t.Fatal("code or shell interpolation in argv")
	}
	entries, _ := os.ReadDir(runner.WorkDir)
	if len(entries) != 0 {
		t.Fatal("snapshot not cleaned")
	}
}
func TestDockerUnavailableNeverExecutesFallback(t *testing.T) {
	runner := NewDockerExecutor("image", "test", t.TempDir())
	runner.Binary = filepath.Join(t.TempDir(), "missing-docker")
	e := catalog(t).List()[0]
	r := runner.Execute(context.Background(), e, fixture(t, e, "correct"), "offline")
	if r.Status != "infrastructure_error" || r.ExitCode != -1 || r.Output != "" {
		t.Fatalf("%#v", r)
	}
}
func TestDockerCancelStillRemovesContainer(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	t.Setenv("PRACTICE_ARGV_FILE", record)
	binary := filepath.Join(dir, "docker")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PRACTICE_ARGV_FILE"
case "$1" in
 start) exec sleep 10;;
 *) exit 0;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	runner := NewDockerExecutor("image", "test", filepath.Join(dir, "snapshots"))
	runner.Binary = binary
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	e := catalog(t).List()[0]
	r := runner.Execute(ctx, e, fixture(t, e, "correct"), "cancel")
	if r.Status != "interrupted" {
		t.Fatalf("%#v", r)
	}
	b, _ := os.ReadFile(record)
	if !strings.Contains(string(b), "rm -f") {
		t.Fatal("cancel skipped cleanup")
	}
}
