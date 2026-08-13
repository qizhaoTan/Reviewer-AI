package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qizhaoTan/Reviewer-AI/internal/config"
	"github.com/qizhaoTan/Reviewer-AI/internal/engine"
	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/provider"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

func main() {
	log.InitDebug()
	configPath := flag.String("config", "", "path to config.json (default: ~/.reviewer/config.json or $REVIEWER_AI_CONFIG)")
	repoDir := flag.String("repo", ".", "path to the git repository to review")
	flag.Parse()

	repoAbs, err := filepath.Abs(*repoDir)
	if err != nil {
		fail("resolve repo path: %v", err)
	}

	ctx := context.Background()
	changes, err := gitdiff.LoadStaged(ctx, repoAbs)
	if err != nil {
		fail("load staged changes: %v", err)
	}
	if len(changes) == 0 {
		fmt.Println("No staged changes to review.")
		return
	}

	branch, err := gitdiff.CurrentBranch(ctx, repoAbs)
	if err != nil {
		fail("resolve current branch: %v", err)
	}

	path := *configPath
	if path == "" {
		path, err = config.DefaultPath()
		if err != nil {
			fail("resolve default config path: %v", err)
		}
	}
	cfgFile, err := config.Load(path)
	if err != nil {
		fail("load config: %v", err)
	}
	modelCfg, err := cfgFile.Resolve()
	if err != nil {
		fail("resolve model config: %v", err)
	}

	llm, err := provider.New(modelCfg.ToProviderConfig())
	if err != nil {
		fail("create provider: %v", err)
	}

	db, err := store.New("")
	if err != nil {
		fail("open run store: %v", err)
	}
	defer db.Close()

	// 只读工具两个阶段共用；收尾工具各不相同——初审用 submit_review 提交意见，
	// 复核用 submit_verdict 给单条意见下裁决。
	readOnlyTools := []tool.ITool{
		tool.ReadFileTool{},
		tool.GlobTool{},
		tool.GrepTool{},
	}
	deps := engine.Deps{
		LLM:                 llm,
		Store:               db,
		Tools:               append(append([]tool.ITool{}, readOnlyTools...), tool.SubmitReviewTool{Changes: changes}),
		CritiqueTools:       append(append([]tool.ITool{}, readOnlyTools...), tool.CritiqueVerdictTool{}),
		CritiqueConcurrency: cfgFile.Critique.ConcurrencyOrDefault(),
		CritiqueMaxTurns:    cfgFile.Critique.MaxTurnsOrDefault(),
	}

	run, err := engine.Run(ctx, deps, repoAbs, branch, changes, modelCfg.Timeout())
	if err != nil {
		fail("%v", err)
	}

	fmt.Print(review.Render(run.Report()))
}

func fail(format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
