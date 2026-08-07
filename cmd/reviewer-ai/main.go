package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qizhaoTan/Reviewer-AI/internal/config"
	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/prompt"
	"github.com/qizhaoTan/Reviewer-AI/internal/provider"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
)

// maxToolLoopIterations 是单次审查运行里 Generate 调用的最大轮数，防止模型无限循环调用工具。
const maxToolLoopIterations = 30

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

	msgs := prompt.BuildInitial(changes)
	tools := []tool.ITool{
		tool.ReadFileTool{},
		tool.GlobTool{},
		tool.GrepTool{},
	}
	toolDefinitions := make([]schema.ToolDefinition, len(tools))
	for i, t := range tools {
		toolDefinitions[i] = t.Definition()
	}

	genCtx, cancel := context.WithTimeout(ctx, modelCfg.Timeout())
	defer cancel()

	for range maxToolLoopIterations {
		resp, err := llm.Generate(genCtx, msgs, toolDefinitions)
		if err != nil {
			fail("generate review: %v", err)
		}
		msgs = append(msgs, *resp)

		if len(resp.ToolCalls) == 0 {
			fmt.Println(resp.Content)
			return
		}

		for _, tc := range resp.ToolCalls {
			log.Info("尝试调用工具", "tool", tc.Name, "arguments", tc.Arguments)
		}

		for _, tc := range resp.ToolCalls {
			var result tool.Result
			if t, err := tool.FindToolByName(tools, tc.Name); err != nil {
				result = tool.Result{
					Output:  fmt.Sprintf("unknown tool %q; no such tool is available. Use only the tools provided in this session.", tc.Name),
					IsError: true,
				}
			} else {
				result = t.Execute(ctx, repoAbs, tc.Arguments)
			}

			if result.IsError {
				log.Error("调用工具失败", "tool", tc.Name, "arguments", tc.Arguments, "output", result.Output)
			} else {
				log.Info("调用工具成功", "tool", tc.Name, "arguments", tc.Arguments, "outputLen", len(result.Output))
			}
			msgs = append(msgs, schema.Message{
				Role:       schema.RoleUser,
				Content:    result.Output,
				ToolCallID: tc.ID,
			})
		}
	}

	fail("exceeded max tool-call iterations (%d) without a final answer", maxToolLoopIterations)
}

func fail(format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
