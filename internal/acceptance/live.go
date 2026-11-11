package acceptance

import (
	"context"
	"slices"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
)

const (
	checkDirect   = "live_multi_turn_direct"
	checkHybrid   = "live_hybrid_classification"
	checkRecovery = "live_workspace_and_recovery"
	checkSummary  = "live_summary"
	checkVectors  = "live_memory_and_rag"
	checkSandbox  = "live_model_docker"
)

var liveCheckNames = []string{checkDirect, checkHybrid, checkRecovery, checkSummary, checkVectors, checkSandbox}

func (v *verifier) live(ctx context.Context) {
	if v.only != "" && !slices.Contains(liveCheckNames, v.only) {
		return
	}
	// An already canceled invocation must neither load credentials nor contact services.
	if ctx.Err() != nil {
		for _, name := range liveCheckNames {
			v.run(ctx, name, "live", nil)
		}
		return
	}
	cfg, err := config.Load(v.config)
	if err != nil {
		name := "live_configuration"
		if v.only != "" {
			name = v.only
		}
		v.run(ctx, name, "live", func() (map[string]any, error) { return nil, err })
		return
	}
	if v.milvus != "" {
		cfg.RAG.MilvusURL = v.milvus
	}
	for _, test := range []struct {
		name string
		fn   func(context.Context, *config.Config) (map[string]any, error)
	}{
		{checkDirect, v.direct},
		{checkHybrid, v.hybrid},
		{checkRecovery, v.recovery},
		{checkSummary, v.summary},
		{checkVectors, v.vectors},
		{checkSandbox, v.sandbox},
	} {
		v.run(ctx, test.name, "live", func() (map[string]any, error) {
			call, cancel := context.WithTimeout(ctx, 4*time.Minute)
			defer cancel()
			return test.fn(call, cfg)
		})
	}
}
