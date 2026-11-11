package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lannokaede/LanAgent/internal/config"
)

func TestCorpusAcceptanceReportsMeasuredCountsAndTampering(t *testing.T) {
	dir := t.TempDir()
	content := []byte(strings.Repeat("x", 600))
	sum := sha256.Sum256(content)
	manifest := map[string]any{"version": 1, "documents": []any{map[string]any{"file": "fixture.md", "sha256": hex.EncodeToString(sum[:]), "url": "https://example.invalid/fixture"}}}
	raw, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0600)
	os.WriteFile(filepath.Join(dir, "fixture.md"), content, 0600)
	metrics, err := verifyCorpus(context.Background(), dir)
	if err != nil || metrics["documents"] != 1 || metrics["chunks"] != 2 {
		t.Fatal(metrics, err)
	}
	os.WriteFile(filepath.Join(dir, "fixture.md"), []byte("tampered"), 0600)
	output := filepath.Join(t.TempDir(), "failure.json")
	err = run(context.Background(), []string{"-mode", "offline", "-only", "corpus_snapshot", "-corpus-dir", dir, "-report", output})
	if err == nil {
		t.Fatal("reported success for invalid corpus")
	}
	var report report
	raw, err = os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &report) != nil || report.Success || len(report.Checks) != 1 || report.Checks[0].Status != "failed" {
		t.Fatal("failure report missing", string(raw))
	}
}
func TestCleanupRejectsUnownedCollectionNames(t *testing.T) {
	for _, name := range []string{"production", "lanagent_personal_memory_v1", ""} {
		if err := dropCollection(context.Background(), &config.Config{}, name); err == nil {
			t.Fatal("accepted unowned collection", name)
		}
	}
}

func TestCanceledLiveChecksAreExplicitlyNotRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(dir, "none.json"))
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("rag:\n  enabled: false\n"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := &verifier{config: dir, report: report{Success: true}}
	v.live(ctx)
	if v.report.Success || len(v.report.Checks) != 6 {
		t.Fatal("canceled run omitted scenarios", v.report)
	}
	for _, check := range v.report.Checks {
		if check.Status != "not_run" {
			t.Fatal("unexpected status", check)
		}
	}
}
