package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePermissionsVersionChecksAndEscape(t *testing.T) {
	dir := t.TempDir()
	w, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	r, _ := w.Registry()
	if _, ok := r.Lookup("write_file"); ok {
		t.Fatal("write enabled implicitly")
	}
	if e = w.RegisterActions(r, true, false); e != nil {
		t.Fatal(e)
	}
	write, _ := r.Lookup("write_file")
	for _, raw := range []string{`{"path":"note.txt","content":"hello","expected_sha256":""}`, `{"path":"note.txt","content":"hello","expected_sha256":""}`} {
		if _, e = write.Execute(context.Background(), raw); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = write.Execute(context.Background(), `{"path":"note.txt","content":"overwrite","expected_sha256":""}`); e == nil {
		t.Fatal("ignored version conflict")
	}
	if _, e = write.Execute(context.Background(), `{"path":"../escape","content":"oops","expected_sha256":""}`); e == nil {
		t.Fatal("escaped root")
	}
	os.MkdirAll(filepath.Join(dir, ".git"), 0700)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("keep metadata"), 0600)
	raw := `{"path":".git/config","content":"changed","expected_sha256":"` + sha("keep metadata") + `"}`
	if _, e = write.Execute(context.Background(), raw); e == nil {
		t.Fatal("modified repository control data")
	}
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("keep"), 0600)
	if e = os.Symlink(target, filepath.Join(dir, "link")); e == nil {
		if _, e = write.Execute(context.Background(), `{"path":"link","content":"oops","expected_sha256":""}`); e == nil {
			t.Fatal("followed symlink")
		}
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("modified outside workspace")
	}
	if _, ok := r.Lookup("run_command"); ok {
		t.Fatal("command enabled implicitly")
	}
}
func TestSkillDiscoveryLoadsOnlyMetadataAndIsBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".lanagent", "skills", "go-review")
	os.MkdirAll(path, 0700)
	os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: go-review\ndescription: Review Go cancellation\n---\nBODY_ONLY_ON_DEMAND"), 0600)
	w, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer w.Close()
	r, _ := w.Registry()
	catalog, e := w.RegisterSkills(r)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(catalog, "BODY_ONLY") || !strings.Contains(catalog, "go-review") {
		t.Fatal("loaded full body eagerly or lost catalog")
	}
	loader, ok := r.Lookup("load_skill")
	if !ok {
		t.Fatal("missing loader")
	}
	text, e := loader.Execute(context.Background(), `{"name":"go-review"}`)
	if e != nil || !strings.Contains(text, "BODY_ONLY") {
		t.Fatal("did not load instructions", e)
	}
	if _, e = loader.Execute(context.Background(), `{"name":"../../secret"}`); e == nil {
		t.Fatal("accepted unknown skill")
	}
	command := commandTool{w: w}
	if _, e = command.Execute(context.Background(), `{"command":"sh -c echo unsafe"}`); e == nil {
		t.Fatal("accepted shell")
	}
}
