package practice

import (
	"context"
	"encoding/json"
	assets "github.com/lannokaede/LanAgent/examples/practice"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in: these tests execute only in disposable Docker containers.
func TestDockerExerciseCases(t *testing.T) {
	if os.Getenv("PRACTICE_DOCKER_TEST") != "1" {
		t.Skip("set PRACTICE_DOCKER_TEST=1 with the built practice image")
	}
	image := os.Getenv("PRACTICE_DOCKER_IMAGE")
	if image == "" {
		image = "lanagent/practice-go:1.26.9-v1"
	}
	base := os.Getenv("PRACTICE_DOCKER_WORK_DIR")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		base = filepath.Join(cache, "lanagent", "practice-tests")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(base, "suite-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	runner := NewDockerExecutor(image, "practice-docker-integration", workspace)
	ctx := context.Background()
	check, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := runner.Health(check); err != nil {
		t.Fatal(err)
	}
	for _, e := range catalog(t).List() {
		b, _ := assets.Files.ReadFile(e.ID + "/" + e.Version + "/cases.json")
		var cases []struct{ Name, File, Expected string }
		_ = json.Unmarshal(b, &cases)
		for _, v := range cases {
			t.Run(e.ID+"/"+v.Name, func(t *testing.T) {
				source, _ := assets.Files.ReadFile(e.ID + "/" + e.Version + "/" + v.File)
				result := runner.Execute(ctx, e, string(source), e.ID+v.Name)
				if result.Status != v.Expected && !(v.Name == "starter" && result.Status == "race_failed") {
					t.Fatalf("want %s got %s: %s %s", v.Expected, result.Status, result.Error, result.Output)
				}
			})
		}
	}
	e, err := catalog(t).Get("context-cancel", "v1")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("timeout", func(t *testing.T) {
		code := `package exercise
import("context";"time")
func Wait(ctx context.Context,delay time.Duration)(string,error){for{} }
`
		out := runner.Execute(ctx, e, code, "timeout")
		if out.Status != "timeout" {
			t.Fatalf("%#v", out)
		}
	})
	t.Run("truncation", func(t *testing.T) {
		code := `package exercise
import("context";"time";"fmt")
func Wait(ctx context.Context,delay time.Duration)(string,error){fmt.Println("` + strings.Repeat("x", 20000) + `");return "ok",nil}
`
		out := runner.Execute(ctx, e, code, "truncation")
		if !out.Truncated || len(out.Output) > MaxOutputBytes {
			t.Fatalf("output not bounded: %#v", out)
		}
	})
	if err := runner.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
}
