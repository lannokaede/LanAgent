package apiconfig

import (
	"strings"
	"testing"
)

func TestImportFormatsAndSeparateEndpointKinds(t *testing.T) {
	inputs := []string{
		`{"base_url":"https://example.test/v1","api_key":"fixture-key","model":"chat-model"}`,
		"llm:\n  base_url: https://example.test/v1\n  api_key: fixture-key\n  model: chat-model\n",
		"export LLM_BASE_URL=https://example.test/v1\nLLM_API_KEY='fixture-key'\nLLM_MODEL=chat-model\n",
		"接口类型：OpenAI Compatible\n\nBase URL：\nhttps://example.test/v1\n\nAPI Key：\nfixture-key\n模型名称：chat-model\n",
	}
	for _, text := range inputs {
		e, err := ParseImport(text, "chat")
		if err != nil || e.BaseURL != "https://example.test/v1" || e.APIKey != "fixture-key" || e.Model != "chat-model" {
			t.Fatalf("import format failed: %v", err)
		}
	}
	e, err := ParseImport("embedding:\n  api_key: embed-fixture\n  model: text-embedding-v4\n  dimensions: 1024\n", "embedding")
	if err != nil || e.Dimensions != 1024 {
		t.Fatal(err)
	}
	for _, text := range []string{"embedding:\n  api_key: private-value\n", "API Key:\n", strings.Repeat("x", 65537), "api_key: [private-value]"} {
		_, err := ParseImport(text, "chat")
		if err == nil || strings.Contains(err.Error(), "private-value") {
			t.Fatal("invalid import accepted or leaked content")
		}
	}
}
