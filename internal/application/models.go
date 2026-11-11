package application

import (
	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/llm"
)

// Every model call (including classification and summaries) crosses a gateway.
// Providers belong to Runtime; gateways add no independent resources.
func (r *Runtime) openModels(cfg *config.Config) (*llm.Gateway, llm.Provider, error) {
	open := func(base, model, key string, classifier bool) (*llm.Gateway, error) {
		timeout := cfg.LLM.Timeout
		if classifier {
			timeout = cfg.Routing.Timeout
		}
		provider, err := llm.NewOpenAIProvider(base, model, key)
		if err != nil {
			return nil, err
		}
		r.resources.release(provider.Close)
		provider.SetTimeout(timeout)
		gateway, err := llm.NewGateway(provider, timeout)
		if err != nil {
			return nil, err
		}
		gateway.SetConcurrency(cfg.LLM.Concurrency)
		if err = gateway.SetRetryPolicy(cfg.LLM.Retry); err != nil {
			return nil, err
		}
		return gateway, nil
	}
	gateway, err := open(cfg.LLM.BaseURL, cfg.LLM.Model, cfg.LLM.APIKey, false)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Routing.Mode != "hybrid" {
		return gateway, nil, nil
	}
	c := cfg.Routing.Classifier
	if c.BaseURL == cfg.LLM.BaseURL && c.Model == cfg.LLM.Model && c.APIKey == cfg.LLM.APIKey {
		return gateway, gateway, nil
	}
	classifier, err := open(c.BaseURL, c.Model, c.APIKey, true)
	return gateway, classifier, err
}
