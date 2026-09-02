package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/qizhaoTan/Reviewer-AI/internal/config"
	"github.com/qizhaoTan/Reviewer-AI/internal/engine"
	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/log"
	"github.com/qizhaoTan/Reviewer-AI/internal/provider"
	"github.com/qizhaoTan/Reviewer-AI/internal/review"
	"github.com/qizhaoTan/Reviewer-AI/internal/store"
	"github.com/qizhaoTan/Reviewer-AI/internal/tool"
	"github.com/qizhaoTan/Reviewer-AI/internal/web"
)

// version 由构建时的 -ldflags "-X main.version=..." 注入；直接 go build 时
// 保持 dev，用来区分本地构建和分发出去的版本。
var version = "dev"

func main() {
	log.InitDebug()

	// 子命令分发：只有 web 一个子命令，用 os.Args[1] 直接判断就够了，
	// 不值得为此引入 cobra。默认（无子命令）走审查流程，保持 `reviewer-ai`
	// 裸跑一次审查的既有用法不变。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "web":
			runWeb(os.Args[2:])
			return
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}
	runReview(os.Args[1:])
}

// runWeb 启动只读的历史记录查看服务。
func runWeb(args []string) {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	addr := fs.String("addr", ":8090", "address for the web viewer to listen on")
	noOpen := fs.Bool("no-open", false, "do not open the viewer in a browser on startup")
	configPath := fs.String("config", "", "path to config.json (default: ~/.reviewer/config.json or $REVIEWER_AI_CONFIG)")
	if err := fs.Parse(args); err != nil {
		fail("parse web flags: %v", err)
	}

	db, err := store.New("")
	if err != nil {
		fail("open run store: %v", err)
	}
	defer db.Close()

	// 交互功能（reply / 增量重审）要调模型，所以需要完整的配置。配置不可用时
	// 只是把 reviewer 留成 nil——页面照常能看历史记录，只是没有回复框和重审
	// 按钮。为了配置文件里少个 API key 就让整个查看服务起不来，是不划算的。
	var reviewer web.Reviewer
	if inter, err := buildInteractive(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "reviewer-ai: 交互功能不可用（%v），仅提供只读浏览\n", err)
	} else {
		inter.Deps.Store = db
		reviewer = inter
	}

	url := "http://localhost" + *addr
	fmt.Fprintf(os.Stderr, "reviewer-ai: web viewer listening on %s\n", url)

	if !*noOpen {
		// 先监听再开浏览器：ListenAndServe 是阻塞的，所以浏览器只能在另一个
		// goroutine 里开。稍等一下让端口先就绪，否则浏览器可能抢在监听之前
		// 打开、拿到一个连接被拒的错误页。
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				// 打不开浏览器不该让服务起不来——URL 已经打在 stderr 上，
				// 手动点开就是了。
				fmt.Fprintf(os.Stderr, "reviewer-ai: could not open browser (%v); open %s manually\n", err, url)
			}
		}()
	}

	if err := web.NewServer(db, reviewer, *addr).ListenAndServe(); err != nil {
		fail("web server: %v", err)
	}
}

// openBrowser 用系统默认浏览器打开 url。
//
// 只认平台自带的命令，不引入第三方库：macOS 的 open、Linux 的 xdg-open、
// Windows 的 rundll32 都是系统自带的，这点小事不值得加一个依赖。
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		name = "xdg-open"
	}
	return exec.Command(name, append(args, url)...).Start()
}

// runReview 执行一次暂存区审查，是不带子命令时的默认行为。
func runReview(args []string) {
	fs := flag.NewFlagSet("review", flag.ExitOnError)
	configPath := fs.String("config", "", "path to config.json (default: ~/.reviewer/config.json or $REVIEWER_AI_CONFIG)")
	repoDir := fs.String("repo", ".", "path to the git repository to review")
	fresh := fs.Bool("fresh", false, "ignore any resumable run for these changes and start a new review from scratch")
	baseRev := fs.String("base", "", "review this branch's own changes against a base revision (e.g. -base=dev), "+
		"equivalent to what `git merge --squash` would stage; default is to review the staged changes")
	if err := fs.Parse(args); err != nil {
		fail("parse flags: %v", err)
	}

	repoAbs, err := filepath.Abs(*repoDir)
	if err != nil {
		fail("resolve repo path: %v", err)
	}

	ctx := context.Background()

	// 配置要在采集变更之前加载：auto_stage 决定了要不要先 `git add -A`，
	// 而那必须发生在 LoadStaged 读取索引之前。
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

	var changes []gitdiff.Change
	if *baseRev != "" {
		// -base 模式刻意不 auto-stage：这条路径要求工作区干净（见 checkBaseReviewable），
		// 而 diff 是按 HEAD 算的，stage 一把只会制造一个 diff 根本不看的脏索引。
		if err := checkBaseReviewable(ctx, repoAbs, *baseRev); err != nil {
			fail("%v", err)
		}
		changes, err = gitdiff.LoadDiffRange(ctx, repoAbs, *baseRev)
		if err != nil {
			fail("load changes against %s: %v", *baseRev, err)
		}
		if len(changes) == 0 {
			fmt.Printf("No changes to review against %s.\n", *baseRev)
			return
		}
	} else {
		if cfgFile.AutoStage {
			if err := gitdiff.StageAll(ctx, repoAbs); err != nil {
				fail("auto-stage changes: %v", err)
			}
		}
		changes, err = gitdiff.LoadStaged(ctx, repoAbs)
		if err != nil {
			fail("load staged changes: %v", err)
		}
		if len(changes) == 0 {
			fmt.Println("No staged changes to review.")
			return
		}
	}

	branch, err := gitdiff.CurrentBranch(ctx, repoAbs)
	if err != nil {
		fail("resolve current branch: %v", err)
	}

	db, err := store.New("")
	if err != nil {
		fail("open run store: %v", err)
	}
	defer db.Close()

	deps, err := buildDeps(cfgFile, modelCfg, changes)
	if err != nil {
		fail("%v", err)
	}
	deps.Store = db
	deps.Fresh = *fresh

	run, err := engine.Run(ctx, deps, repoAbs, branch, *baseRev, changes, modelCfg.Timeout())
	if err != nil {
		fail("%v", err)
	}

	fmt.Print(review.Render(run.Report()))
}

// checkBaseReviewable 在按 base 审查之前做两项前置校验。
//
// 一是工作区必须干净：base...HEAD 是按 HEAD 这个提交算出来的，而模型用
// read_file 读到的是工作区里的文件。工作区有未提交的改动，这两者就对不上，
// 模型会拿着一份跟 diff 不一致的代码下判断。
//
// 二是必须能干净地合进 base：合不上就说明这份 diff 还不是最终要进 base 的内容，
// 冲突解完代码会变，现在审等于审一个不存在的版本。
//
// 冲突检测依赖 git 2.38+ 的只读 merge-tree；版本不够时降级为跳过并提示，
// 不让 git 版本旧变成整个功能不可用。
func checkBaseReviewable(ctx context.Context, repoAbs, baseRev string) error {
	if _, err := gitdiff.ResolveRevision(ctx, repoAbs, baseRev); err != nil {
		return err
	}

	dirty, err := gitdiff.WorkTreeDirtyPaths(ctx, repoAbs)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("work tree is not clean (%s); reviewing against %s compares committed history, "+
			"so commit or stash these changes first", summarizePaths(dirty), baseRev)
	}

	conflicts, err := gitdiff.MergeConflicts(ctx, repoAbs, baseRev)
	if errors.Is(err, gitdiff.ErrMergeTreeUnsupported) {
		fmt.Fprintf(os.Stderr, "reviewer-ai: 跳过合并冲突预检（需要 git 2.38+）\n")
		return nil
	}
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("merging into %s would conflict in %s; resolve the conflicts first, "+
			"since the code that lands in %s will differ from what is reviewed here",
			baseRev, summarizePaths(conflicts), baseRev)
	}
	return nil
}

// summarizePaths 把路径列表压成一行，超过 maxListedPaths 条时只列前几条加计数——
// 一个分支冲突上百个文件时，把它们全打出来会把真正的错误信息冲走。
func summarizePaths(paths []string) string {
	const maxListedPaths = 5
	if len(paths) <= maxListedPaths {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:maxListedPaths], ", "), len(paths)-maxListedPaths)
}

func fail(format string, args ...any) {
	log.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}

// readOnlyTools 是三个阶段（初审 / 复核 / reply）共用的只读工具。
// 每次都新建一份切片而不是共享一个包级变量：调用方会往返回值上 append
// 各自的收尾工具，共享底层数组会让两次 append 互相覆盖。
func readOnlyTools() []tool.ITool {
	return []tool.ITool{
		tool.ReadFileTool{},
		tool.GlobTool{},
		tool.GrepTool{},
	}
}

// buildDeps 组装跑一次审查所需的依赖。Store 由调用方填——两条入口
// （命令行审查、Web 交互）各自管理数据库的生命周期。
//
// 收尾工具三个阶段各不相同：初审用 submit_review 提交意见，复核用
// submit_verdict 给单条意见下裁决，reply 用 withdraw_finding 撤回。
func buildDeps(cfgFile *config.File, modelCfg config.ModelConfig, changes []gitdiff.Change) (engine.Deps, error) {
	llm, err := provider.New(modelCfg.ToProviderConfig())
	if err != nil {
		return engine.Deps{}, fmt.Errorf("create provider: %w", err)
	}
	return engine.Deps{
		LLM:                 llm,
		Tools:               append(readOnlyTools(), tool.SubmitReviewTool{Changes: changes}),
		CritiqueTools:       append(readOnlyTools(), tool.CritiqueVerdictTool{}),
		CritiqueConcurrency: cfgFile.Critique.ConcurrencyOrDefault(),
		CritiqueMaxTurns:    cfgFile.Critique.MaxTurnsOrDefault(),
		LanguagePrompt:      cfgFile.LanguagePromptOrDefault(),
	}, nil
}

// buildInteractive 为 Web 侧装配交互能力。返回的 Interactive 还缺 Deps.Store，
// 由调用方填上自己打开的那个数据库。
//
// Deps.Tools 这里留着 submit_review 但 Changes 为空：增量重审时 engine.Run
// 会用到它，而那时真正的 changes 由 Rereview 现场采集——工具实例上的 Changes
// 只用于 anchor 反推行号，留空时意见降级成文件级，不会出错。
func buildInteractive(configPath string) (*engine.Interactive, error) {
	path := configPath
	if path == "" {
		var err error
		path, err = config.DefaultPath()
		if err != nil {
			return nil, fmt.Errorf("resolve default config path: %w", err)
		}
	}
	cfgFile, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	modelCfg, err := cfgFile.Resolve()
	if err != nil {
		return nil, fmt.Errorf("resolve model config: %w", err)
	}
	deps, err := buildDeps(cfgFile, modelCfg, nil)
	if err != nil {
		return nil, err
	}
	return &engine.Interactive{
		Deps:       deps,
		ReplyTools: append(readOnlyTools(), tool.WithdrawFindingTool{}),
		Timeout:    modelCfg.Timeout(),
		AutoStage:  cfgFile.AutoStage,
	}, nil
}
