package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalOverridesEnvironmentPrecedenceAndLimits(t *testing.T) {
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(t.TempDir(), "none.json"))
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
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("rag:\n  enabled: false\n"), 0600)
	t.Setenv("LLM_API_KEY", "")
	if _, e := Load(dir); e != nil {
		t.Fatal(e)
	}
}

func TestRoutingInheritanceRetryDefaultsAndValidation(t *testing.T) {
	t.Setenv("LANAGENT_API_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("LLM_BASE_URL", "http://localhost:1234/v1")
	t.Setenv("LLM_MODEL", "main-model")
	t.Setenv("LLM_API_KEY", "main-key")
	t.Setenv("ROUTER_MODEL", "classifier-model")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	os.WriteFile(path, []byte("routing:\n  mode: hybrid\n"), 0600)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Classifier.Model != "classifier-model" || cfg.Routing.Classifier.APIKey != "main-key" || cfg.Routing.Classifier.BaseURL != cfg.LLM.BaseURL || cfg.LLM.Retry.MaxAttempts != 3 {
		t.Fatal("routing inheritance or retry defaults lost")
	}
	for _, body := range []string{"routing:\n  mode: unknown\n", "routing:\n  timeout: 0s\n", "llm:\n  retry:\n    max_attempts: 0\n", "llm:\n  retry:\n    base_delay: 0s\n"} {
		os.WriteFile(path, []byte(body), 0600)
		if _, err := Load(dir); err == nil {
			t.Fatal("accepted invalid policy", body)
		}
	}
}
