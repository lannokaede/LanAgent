package personal

import (
	"encoding/json"
	"github.com/lannokaede/LanAgent/internal/harness"
	"github.com/lannokaede/LanAgent/internal/llm"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCatalogPendingOnlyProfilesAndSidecars(t *testing.T) {
	dir, work := t.TempDir(), t.TempDir()
	s, e := Open(dir, "pending-session", work)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveLaunch(Launch{Workspace: work, Config: "config", Write: true, Summary: true, Rounds: 10, Timeout: time.Minute}); e != nil {
		t.Fatal(e)
	}
	cp := harness.Checkpoint{Version: 1, RunID: "run", Phase: "tools", Running: "execution", Messages: []llm.Message{{Role: llm.RoleUser, Content: "调查\x1b[31m题目"}}}
	if e = s.savePending(cp); e != nil {
		t.Fatal(e)
	}
	s.Close()
	os.WriteFile(filepath.Join(dir, "ignored.interview.json"), []byte(`{}`), 0600)
	os.WriteFile(filepath.Join(dir, "corrupt.json"), []byte(`{`), 0600)
	all, e := List(dir)
	if e != nil || len(all) != 1 {
		t.Fatal(all, e)
	}
	v := all[0]
	if v.Status != "需核对" || v.Launch == nil || !v.Launch.Write || strings.ContainsRune(v.Title, '\x1b') {
		t.Fatal(v)
	}
	// A committed snapshot supersedes a leftover pending file.
	b, _ := json.Marshal(snapshot{Version: 1, Workspace: work, LastRun: "run", Messages: []llm.Message{{Role: llm.RoleUser, Content: "saved"}, {Role: llm.RoleAssistant, Content: "done"}}})
	os.WriteFile(filepath.Join(dir, "pending-session.json"), b, 0600)
	all, e = List(dir)
	if e != nil || all[0].Status != "已保存" {
		t.Fatal(all, e)
	}
}
