package acceptance_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lannokaede/LanAgent/internal/acceptance"
)

type recordedReport struct {
	Version int
	Mode    string
	Success bool
	Checks  []struct {
		Name, Mode, Status, Error string
		Metrics                   map[string]any
	}
}

func TestRunReportsCorpusCountsAndChecksumFailures(t *testing.T) {
	root := repository(t)
	dir := filepath.Join(root, "corpus")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte(strings.Repeat("x", 600))
	sum := sha256.Sum256(content)
	manifest := map[string]any{"version": 1, "documents": []any{map[string]any{"file": "fixture.md", "sha256": hex.EncodeToString(sum[:]), "url": "https://example.invalid/fixture"}}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "manifest.json"), raw)
	write(t, filepath.Join(dir, "fixture.md"), content)
	// Selecting only the corpus must not load missing live configuration.
	opts := acceptance.Options{RootDir: root, Mode: "all", Only: "corpus_snapshot", CorpusDir: "corpus", ReportPath: "results/corpus.json"}
	var output bytes.Buffer
	if err := acceptance.Run(context.Background(), opts, &output); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, opts.ReportPath)
	r := readReport(t, path)
	if !r.Success || r.Version != 1 || r.Mode != "all" || len(r.Checks) != 1 {
		t.Fatalf("unexpected report: %+v", r)
	}
	c := r.Checks[0]
	if c.Status != "passed" || c.Metrics["documents"] != float64(1) || c.Metrics["chunks"] != float64(2) {
		t.Fatalf("missing measured corpus evidence: %+v", c)
	}
	manifestSum := sha256.Sum256(raw)
	if c.Metrics["manifest_sha256"] != hex.EncodeToString(manifestSum[:]) {
		t.Fatal("report does not identify the verified snapshot")
	}
	if !strings.Contains(output.String(), "corpus_snapshot: passed") {
		t.Fatalf("progress was not written to supplied output: %s", output.String())
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("report permissions: %v, %v", info, err)
	}
	write(t, filepath.Join(dir, "fixture.md"), []byte("tampered"))
	if err := acceptance.Run(context.Background(), opts, nil); err == nil {
		t.Fatal("reported success for invalid corpus")
	}
	r = readReport(t, path)
	if r.Success || len(r.Checks) != 1 || r.Checks[0].Status != "failed" || !strings.Contains(r.Checks[0].Error, "checksum") {
		t.Fatalf("failure report missing: %+v", r)
	}
	md, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".md")
	if err != nil || !bytes.Contains(md, []byte("| corpus_snapshot | offline | failed |")) {
		t.Fatalf("failure Markdown missing: %s, %v", md, err)
	}
}

func TestRunCanceledLiveChecksAreExplicitlyNotRun(t *testing.T) {
	root := repository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := acceptance.Options{RootDir: root, Mode: "live", ReportPath: "canceled.json"}
	err := acceptance.Run(ctx, opts, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	r := readReport(t, filepath.Join(root, opts.ReportPath))
	if r.Success || len(r.Checks) != 6 {
		t.Fatalf("canceled run omitted scenarios: %+v", r)
	}
	for _, c := range r.Checks {
		if c.Status != "not_run" || c.Error == "" || c.Metrics != nil {
			t.Fatalf("unexecuted scenario misreported: %+v", c)
		}
	}
}

func TestRunLiveConfigurationFailureStillWritesSelectedCheck(t *testing.T) {
	root := repository(t)
	opts := acceptance.Options{RootDir: root, Mode: "live", Only: "live_multi_turn_direct", ConfigDir: "missing", ReportPath: "failed.json"}
	if err := acceptance.Run(context.Background(), opts, nil); err == nil {
		t.Fatal("missing configuration accepted")
	}
	r := readReport(t, filepath.Join(root, opts.ReportPath))
	if r.Success || len(r.Checks) != 1 || r.Checks[0].Name != opts.Only || r.Checks[0].Status != "failed" {
		t.Fatalf("configuration failure missing: %+v", r)
	}
}

func TestRunRejectsUnknownSelectionWithoutCreatingRuntime(t *testing.T) {
	root := repository(t)
	err := acceptance.Run(context.Background(), acceptance.Options{RootDir: root, Only: "not-a-check"}, nil)
	if err == nil || !strings.Contains(err.Error(), "no checks matched") {
		t.Fatalf("invalid selection accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".runtime")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid options created runtime evidence: %v", err)
	}
}

func TestRunModelFailureCleansCredentialsAndPreservesSourceProfile(t *testing.T) {
	root := repository(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"fixture authentication failure"}}`))
	}))
	defer server.Close()
	for key, value := range map[string]string{
		"LANAGENT_API_CONFIG": filepath.Join(root, "no-api.json"),
		"LLM_BASE_URL":        server.URL, "LLM_MODEL": "fixture-model", "LLM_API_KEY": "secret-fixture-value",
		"ROUTER_BASE_URL": "", "ROUTER_MODEL": "", "ROUTER_API_KEY": "",
	} {
		t.Setenv(key, value)
	}
	profile := filepath.Join(root, "personal-profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	primary, local := []byte("rag:\n  enabled: false\n"), []byte("llm:\n  model: personal-model\n")
	write(t, filepath.Join(profile, "config.yaml"), primary)
	write(t, filepath.Join(profile, "config.local.yaml"), local)
	opts := acceptance.Options{RootDir: root, Mode: "live", Only: "live_multi_turn_direct", ConfigDir: profile, ReportPath: "failed-model.json"}
	if err := acceptance.Run(context.Background(), opts, nil); err == nil || calls.Load() == 0 {
		t.Fatalf("model failure not exercised: calls=%d, error=%v", calls.Load(), err)
	}
	r := readReport(t, filepath.Join(root, opts.ReportPath))
	if r.Success || len(r.Checks) != 1 || r.Checks[0].Status != "failed" {
		t.Fatalf("model failure report missing: %+v", r)
	}
	for name, before := range map[string][]byte{"config.yaml": primary, "config.local.yaml": local} {
		after, err := os.ReadFile(filepath.Join(profile, name))
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("source profile %s changed: %v", name, err)
		}
	}
	remaining, err := filepath.Glob(filepath.Join(root, ".runtime", "harness-validation", "run-*", "direct", "config.local.yaml"))
	if err != nil || len(remaining) != 0 {
		t.Fatalf("temporary credentials retained: %v, %v", remaining, err)
	}
	logs, err := filepath.Glob(filepath.Join(root, ".runtime", "harness-validation", "run-*", "direct", "cli.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("private failure evidence not retained: %v, %v", logs, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, opts.ReportPath))
	if err != nil || bytes.Contains(raw, []byte("secret-fixture-value")) {
		t.Fatalf("portable report disclosed credentials: %v", err)
	}
}

func repository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "go.mod"), []byte("module acceptance-fixture\n"))
	return root
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readReport(t *testing.T, path string) recordedReport {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report recordedReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("invalid report: %s, %v", raw, err)
	}
	return report
}
