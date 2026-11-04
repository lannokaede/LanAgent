package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalOverridesEnvironmentPrecedenceAndLimits(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("llm:\n  model: base-model\n"), 0600)
	os.WriteFile(filepath.Join(dir, "config.local.yaml"), []byte("llm:\n  model: local-model\n  api_key: fixture-local-key\n"), 0600)
	t.Setenv("LLM_MODEL", "env-model")
	t.Setenv("LLM_API_KEY", "fixture-env-key")
	cfg, e := Load(dir)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.LLM.Model != "env-model" || cfg.LLM.APIKey != "fixture-env-key" || cfg.Context.Window != 32768 {
		t.Fatal("incorrect override precedence")
	}
	os.WriteFile(filepath.Join(dir, "config.local.yaml"), []byte("llm:\n  concurrency: 0\n"), 0600)
	if _, e = Load(dir); e == nil {
		t.Fatal("accepted invalid concurrency")
	}
}
func TestOfflineProfileDoesNotRequireChatCredential(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("rag:\n  enabled: false\n"), 0600)
	t.Setenv("LLM_API_KEY", "")
	if _, e := Load(dir); e != nil {
		t.Fatal(e)
	}
}
