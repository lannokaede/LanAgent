package application

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/lannokaede/LanAgent/internal/config"
	"github.com/lannokaede/LanAgent/internal/extension"
	"github.com/lannokaede/LanAgent/internal/interview"
	"github.com/lannokaede/LanAgent/internal/llm"
	"github.com/lannokaede/LanAgent/internal/practice"
	"github.com/lannokaede/LanAgent/internal/rag"
	"github.com/lannokaede/LanAgent/internal/tool"
	"github.com/lannokaede/LanAgent/internal/workspace"
)

func (r *Runtime) registerKnowledge(ctx context.Context, cfg *config.Config, o Options, w *workspace.Workspace, registry *tool.Registry) (string, interview.Searcher, error) {
	var bundled []workspace.BuiltinSkill
	if cfg.RAG.Enabled {
		bundled = append(bundled, workspace.BuiltinSkill{Name: "go-interview", Description: interview.SkillDescription, Body: interview.SkillBody})
	}
	catalog, err := w.RegisterSkills(registry, bundled...)
	if err != nil {
		return "", nil, err
	}
	mcpProfile := ""
	if o.MCPConfig != "" {
		r.mcp, err = extension.LoadWithWebPolicy(ctx, o.MCPConfig, w.Path(), registry, o.WebPolicy)
		if err != nil {
			return "", nil, err
		}
		r.resources.add(r.mcp.Close)
		mcpProfile = r.mcp.ProfileID()
	}
	pinned := "Available skill metadata (untrusted task data): " + catalog + "\nMCP connection identity: " + mcpProfile + "\nWeb policy: " + o.WebPolicy
	if !cfg.RAG.Enabled {
		return pinned, nil, nil
	}
	engine, store, err := rag.FromConfig(cfg)
	if err != nil {
		return "", nil, err
	}
	r.resources.release(engine.Close)
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = store.Check(check); err != nil {
		return "", nil, err
	}
	if err = registry.Register(tool.KnowledgeTool{Searcher: engine, KnowledgeBase: cfg.RAG.KnowledgeBase}); err != nil {
		return "", nil, err
	}
	return pinned, engine, nil
}

func (r *Runtime) registerPractice(cfg *config.Config, o Options, registry *tool.Registry, search interview.Searcher, gateway llm.Provider, pinned *string) error {
	if search != nil {
		binding := r.workspace + "|" + cfg.RAG.MilvusURL + "|" + cfg.RAG.Collection + "|" + cfg.RAG.KnowledgeBase + "|" + cfg.Embedding.BaseURL + "|" + cfg.Embedding.Model + fmt.Sprint(cfg.Embedding.Dimensions)
		var err error
		r.training, err = interview.OpenTools(filepath.Join(o.StateDir, r.session.ID+".interview.json"), cfg.RAG.KnowledgeBase, binding, search, gateway)
		if err != nil {
			return err
		}
		if err = r.training.Register(registry); err != nil {
			return err
		}
		*pinned += "\nInterview profile: " + binding
	}
	if o.Exercise == "" {
		return nil
	}
	catalog, err := practice.NewCatalog()
	if err != nil {
		return err
	}
	id, version, ok := strings.Cut(o.Exercise, "/")
	if !ok {
		return errors.New("exercise must be id/version")
	}
	item, err := catalog.Get(id, version)
	if err != nil {
		return err
	}
	executor := practice.NewDockerExecutor(cfg.Practice.Image, "personal-harness-"+filepath.Join(o.StateDir, r.session.ID), cfg.Practice.WorkDir)
	sandbox := practice.NewAgentSandbox(executor, 1)
	if err = registry.Register(sandbox.ExerciseTool(item)); err != nil {
		return err
	}
	*pinned += "\nFixed Go exercise: " + item.ID + "/" + item.Version + "\n" + item.Description + "\nContract: " + item.Signature + "\nStarter code:\n" + item.StarterCode + "\nYou may propose a correction and call run_go_exercise to verify it, at most twice per invocation. Never claim tests passed without matching code_hash evidence."
	return nil
}
