// Package config loads an explicit profile and optional ignored local overrides.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/observability/telemetry"
	"github.com/spf13/viper"
)

type ContextBudget struct {
	Window        int `mapstructure:"window"`
	OutputReserve int `mapstructure:"output_reserve"`
	ToolReserve   int `mapstructure:"tool_reserve"`
	SummaryTokens int `mapstructure:"summary_tokens"`
}

func (c ContextBudget) Validate() error {
	if c.Window < 256 || c.OutputReserve < 1 || c.ToolReserve < 0 || c.SummaryTokens < 0 || c.Window-c.OutputReserve-c.ToolReserve < 128 {
		return fmt.Errorf("invalid context budget")
	}
	return nil
}

type LLM struct {
	BaseURL     string          `mapstructure:"base_url"`
	Model       string          `mapstructure:"model"`
	APIKey      string          `mapstructure:"-"`
	Timeout     time.Duration   `mapstructure:"timeout"`
	Concurrency int             `mapstructure:"concurrency"`
	Retry       llm.RetryPolicy `mapstructure:"retry"`
}
type Classifier struct {
	BaseURL string `mapstructure:"base_url"`
	Model   string `mapstructure:"model"`
	APIKey  string `mapstructure:"-"`
}
type Routing struct {
	Mode       string        `mapstructure:"mode"`
	Timeout    time.Duration `mapstructure:"timeout"`
	Classifier Classifier    `mapstructure:"classifier"`
}
type Embedding struct {
	BaseURL    string `mapstructure:"base_url"`
	Model      string `mapstructure:"model"`
	APIKey     string `mapstructure:"-"`
	Dimensions int    `mapstructure:"dimensions"`
}
type RAG struct {
	Enabled       bool   `mapstructure:"enabled"`
	MilvusURL     string `mapstructure:"milvus_url"`
	Collection    string `mapstructure:"collection"`
	KnowledgeBase string `mapstructure:"knowledge_base"`
	MilvusToken   string `mapstructure:"-"`
}
type Practice struct {
	Image   string `mapstructure:"image"`
	WorkDir string `mapstructure:"work_dir"`
}
type Config struct {
	Context   ContextBudget    `mapstructure:"context"`
	LLM       LLM              `mapstructure:"llm"`
	Embedding Embedding        `mapstructure:"embedding"`
	RAG       RAG              `mapstructure:"rag"`
	Practice  Practice         `mapstructure:"practice"`
	Telemetry telemetry.Config `mapstructure:"telemetry"`
	Routing   Routing          `mapstructure:"routing"`
}

func Load(dir string) (*Config, error) {
	if dir == "" {
		dir = "configs/harness"
	}
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(dir)
	v.SetDefault("routing.mode", "rules")
	v.SetDefault("routing.timeout", "3s")
	v.SetDefault("llm.retry.max_attempts", 3)
	v.SetDefault("llm.retry.base_delay", "250ms")
	for key, value := range map[string]any{"context.window": 32768, "context.output_reserve": 4096, "context.tool_reserve": 2048, "context.summary_tokens": 512, "llm.timeout": "45s", "llm.concurrency": 1, "practice.image": "lanagent/practice-go:1.26.9-v1", "practice.work_dir": ".runtime/practice", "telemetry.file": ".runtime/traces.jsonl"} {
		v.SetDefault(key, value)
	}
	if e := v.ReadInConfig(); e != nil {
		return nil, fmt.Errorf("read profile: %w", e)
	}
	apiPath, e := APISettingsPath()
	if e != nil {
		return nil, e
	}
	shared, e := ReadAPISettings(apiPath)
	if e != nil {
		return nil, e
	}
	if e = v.MergeConfigMap(shared.defaults()); e != nil {
		return nil, fmt.Errorf("merge local API settings")
	}
	v.SetConfigName("config.local")
	if e := v.MergeInConfig(); e != nil {
		if _, ok := e.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read local profile: %w", e)
		}
	}
	var c Config
	if e := v.Unmarshal(&c); e != nil {
		return nil, fmt.Errorf("decode profile: %w", e)
	}
	c.LLM.APIKey = v.GetString("llm.api_key")
	c.Embedding.APIKey = v.GetString("embedding.api_key")
	c.RAG.MilvusToken = v.GetString("rag.milvus_token")
	c.Routing.Classifier.APIKey = v.GetString("routing.classifier.api_key")
	for key, target := range map[string]*string{"LLM_BASE_URL": &c.LLM.BaseURL, "LLM_MODEL": &c.LLM.Model, "LLM_API_KEY": &c.LLM.APIKey, "EMBEDDING_BASE_URL": &c.Embedding.BaseURL, "EMBEDDING_MODEL": &c.Embedding.Model, "EMBEDDING_API_KEY": &c.Embedding.APIKey, "MILVUS_URL": &c.RAG.MilvusURL, "MILVUS_TOKEN": &c.RAG.MilvusToken} {
		if value := os.Getenv(key); value != "" {
			*target = value
		}
	}
	for key, target := range map[string]*string{"ROUTER_BASE_URL": &c.Routing.Classifier.BaseURL, "ROUTER_MODEL": &c.Routing.Classifier.Model, "ROUTER_API_KEY": &c.Routing.Classifier.APIKey} {
		if value := os.Getenv(key); value != "" {
			*target = value
		}
	}
	if c.Routing.Classifier.BaseURL == "" {
		c.Routing.Classifier.BaseURL = c.LLM.BaseURL
	}
	if c.Routing.Classifier.Model == "" {
		c.Routing.Classifier.Model = c.LLM.Model
	}
	if c.Routing.Classifier.APIKey == "" {
		c.Routing.Classifier.APIKey = c.LLM.APIKey
	}
	if (c.Routing.Mode != "rules" && c.Routing.Mode != "hybrid") || c.Routing.Timeout <= 0 || c.Routing.Timeout > 30*time.Second {
		return nil, fmt.Errorf("invalid routing configuration")
	}
	if err := c.LLM.Retry.Validate(); err != nil {
		return nil, err
	}
	if e := c.Context.Validate(); e != nil {
		return nil, e
	}
	if c.LLM.Timeout <= 0 || c.LLM.Concurrency < 1 || c.LLM.Concurrency > 16 {
		return nil, fmt.Errorf("invalid model timeout or concurrency")
	}
	// Keys are checked by the adapter that uses them: offline preparation and
	// embedding-only operations must not require a chat-model credential.
	return &c, nil
}
