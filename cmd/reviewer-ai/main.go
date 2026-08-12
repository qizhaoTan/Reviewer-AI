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

	deps := engine.Deps{
		LLM:   llm,
		Store: db,
		Tools: []tool.ITool{
			tool.ReadFileTool{},
			tool.GlobTool{},
			tool.GrepTool{},
			tool.SubmitReviewTool{Changes: changes},
		},
	}

	run, report, err := engine.Run(ctx, deps, repoAbs, branch, changes, modelCfg.Timeout())
	if err != nil {
		fail("%v", err)
	}
	if report == nil {
		// 命中了历史 completed 记录，但 Findings 还没有持久化（阶段二 2.6 会补上）。
		fmt.Printf("This changeset was already reviewed (run %s), but stored findings are not available yet.\n", run.ID)
		return
	}

	fmt.Print(review.Render(*report))
}

func fail(format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
