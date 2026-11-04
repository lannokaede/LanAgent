package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lannokaede/LanAgent/internal/config"
)

func isolated(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api.local.json")
	t.Setenv("LANAGENT_API_CONFIG", p)
	return p
}
func TestInteractiveSetupChecksAndNeverPrintsKey(t *testing.T) {
	path := isolated(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("request uses wrong endpoint or credentials")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	var out bytes.Buffer
	input := server.URL + "/v1/chat/completions\nfixture-secret\nfixture-model\n"
	if e := run(context.Background(), nil, strings.NewReader(input), &out); e != nil {
		t.Fatal(e)
	}
	saved, e := config.ReadAPISettings(path)
	if e != nil || saved.Chat.BaseURL != server.URL+"/v1" || saved.Chat.APIKey != "fixture-secret" || calls.Load() != 1 {
		t.Fatal("setup did not normalize, verify and save")
	}
	if strings.Contains(out.String(), "fixture-secret") {
		t.Fatal("key printed during setup")
	}
	out.Reset()
	if e = run(context.Background(), []string{"-show"}, strings.NewReader(""), &out); e != nil || strings.Contains(out.String(), "fixture-secret") {
		t.Fatal("show leaked key")
	}
}
func TestPasteImportListsModelsAndPreservesEmbedding(t *testing.T) {
	path := isolated(t)
	config.WriteAPISettings(path, config.APISettings{Version: 1, Embedding: config.APIEndpoint{Model: "text-embedding-v4", APIKey: "embed-fixture", Dimensions: 1024}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`)
	}))
	defer server.Close()
	input := "Base URL：\n" + server.URL + "\nAPI Key：\npaste-fixture\n/save\n2\n"
	var out bytes.Buffer
	if e := run(context.Background(), []string{"-import", "-", "-skip-check"}, strings.NewReader(input), &out); e != nil {
		t.Fatal(e)
	}
	saved, e := config.ReadAPISettings(path)
	if e != nil || saved.Chat.Model != "model-b" || saved.Embedding.APIKey != "embed-fixture" {
		t.Fatal("selected wrong model or overwrote embedding")
	}
	if strings.Contains(out.String(), "paste-fixture") {
		t.Fatal("paste key echoed")
	}
}
func TestFailedCheckKeepsSavedCredentialsAndHidesProviderBody(t *testing.T) {
	path := isolated(t)
	config.WriteAPISettings(path, config.APISettings{Version: 1, Chat: config.APIEndpoint{BaseURL: "https://old.example/v1", Model: "old", APIKey: "old-fixture"}})
	before, _ := os.ReadFile(path)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "rejected private-fixture-secret")
	}))
	defer server.Close()
	importFile := filepath.Join(t.TempDir(), "input.json")
	os.WriteFile(importFile, []byte(fmt.Sprintf(`{"base_url":%q,"model":"new","api_key":"private-fixture-secret"}`, server.URL)), 0600)
	var out bytes.Buffer
	e := run(context.Background(), []string{"-import", importFile}, strings.NewReader(""), &out)
	after, _ := os.ReadFile(path)
	if e == nil || !strings.Contains(e.Error(), "401") || strings.Contains(e.Error()+out.String(), "private-fixture-secret") || !bytes.Equal(before, after) {
		t.Fatalf("err_nil=%v status401=%v leaked=%v changed=%v", e == nil, e != nil && strings.Contains(e.Error(), "401"), e != nil && strings.Contains(e.Error()+out.String(), "private-fixture-secret"), !bytes.Equal(before, after))
	}
}
