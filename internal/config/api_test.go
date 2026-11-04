package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSharedAPIPrecedenceAndPersistence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private", "api.local.json")
	t.Setenv("LANAGENT_API_CONFIG", path)
	settings := APISettings{Version: 1, Chat: APIEndpoint{BaseURL: "https://shared.example/v1", Model: "shared", APIKey: "shared-fixture"}, Embedding: APIEndpoint{Model: "text-embedding-v4", APIKey: "embedding-fixture", Dimensions: 1024}}
	if e := WriteAPISettings(path, settings); e != nil {
		t.Fatal(e)
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("credential file is not private")
	}
	dir := filepath.Join(root, "profile")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("llm:\n  model: base\n  base_url: https://base.example/v1\n"), 0600)
	os.WriteFile(filepath.Join(dir, "config.local.yaml"), []byte("llm:\n  model: project-local\n  api_key: project-fixture\n"), 0600)
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("LLM_MODEL", "")
	t.Setenv("LLM_API_KEY", "")
	c, e := Load(dir)
	if e != nil {
		t.Fatal(e)
	}
	if c.LLM.Model != "project-local" || c.LLM.APIKey != "project-fixture" || c.LLM.BaseURL != "https://shared.example/v1" {
		t.Fatal("shared/project precedence is incorrect")
	}
	t.Setenv("LLM_MODEL", "environment")
	c, e = Load(dir)
	if e != nil || c.LLM.Model != "environment" {
		t.Fatal("environment override ignored")
	}
	copy, e := ReadAPISettings(path)
	if e != nil || copy.Embedding.APIKey != "embedding-fixture" {
		t.Fatal("other endpoint was lost")
	}
	os.WriteFile(path, []byte(`{"version":1,"llm":{"api_key":"private-value"}`), 0600)
	_, e = ReadAPISettings(path)
	if e == nil || strings.Contains(e.Error(), "private-value") {
		t.Fatal("invalid setting leaked data")
	}
}
func TestNormalizePastedAPIEndpoints(t *testing.T) {
	cases := map[string]string{"https://example.test/api/v1/chat/completions/": "https://example.test/api/v1", "http://localhost:1234/v1/": "http://localhost:1234/v1", "https://example.test/v1/models": "https://example.test/v1", "https://example.test": "https://example.test"}
	for in, want := range cases {
		got, e := NormalizeAPIURL(in)
		if e != nil || got != want {
			t.Fatalf("URL normalization: %v", e)
		}
	}
	for _, in := range []string{"file:///tmp/key", "https://user:key@example.test/v1", "https://example.test/v1?api_key=private"} {
		if _, e := NormalizeAPIURL(in); e == nil {
			t.Fatal("accepted unsupported URL")
		}
	}
}
