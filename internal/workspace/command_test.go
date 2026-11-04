package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitDiagnosticsDoNotRunRepositoryFilters(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git fixture: %v %s", e, b)
		}
	}
	git("init", "-q")
	git("config", "user.name", "fixture")
	git("config", "user.email", "fixture@example.invalid")
	os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("before\n"), 0600)
	git("add", "sample.txt")
	git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	git("config", "filter.probe.clean", "touch filter-executed; cat")
	git("config", "filter.probe.required", "true")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt filter=probe\n"), 0600)
	os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("after\n"), 0600)
	w, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	if _, e = (commandTool{w: w}).Execute(context.Background(), `{"command":"git_diff"}`); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(dir, "filter-executed")); !os.IsNotExist(e) {
		t.Fatal("fixed diagnostic executed a repository-supplied command")
	}
}
