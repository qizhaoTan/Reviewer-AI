package gitdiff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// LoadDiffRange 返回从 base 与 HEAD 的合并基点（merge base）到 HEAD 的文件级
// 变更，等价于 `git diff base...HEAD`。
//
// 用三点而非两点 diff 是刻意的：两点 diff（`git diff base HEAD`）会把"base 上
// 有而 HEAD 上没有"的提交也算进来，表现为一堆凭空出现的删除行——那是别人在
// base 上的工作，不该出现在本分支的审查范围里。三点 diff 只包含当前分支自己
// 引入的改动，正是 `git merge --squash` 到 base 之后暂存区里会有的内容。
func LoadDiffRange(ctx context.Context, repoDir, base string) ([]Change, error) {
	spec := base + "...HEAD"
	entries, err := git(ctx, repoDir, "diff", "--name-status", "-z", "--no-renames", spec, "--")
	if err != nil {
		return nil, fmt.Errorf("list changes against %q: %w", base, err)
	}

	fields := bytes.Split(entries, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("parse changes against %q: expected status/path pairs, got %d fields", base, len(fields))
	}

	changes := make([]Change, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		status := string(fields[i])
		path := string(fields[i+1])
		patch, err := git(ctx, repoDir,
			"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=3", spec, "--", path,
		)
		if err != nil {
			return nil, fmt.Errorf("read patch for %q against %q: %w", path, base, err)
		}
		changes = append(changes, Change{Status: status, Path: path, Patch: string(patch)})
	}
	return changes, nil
}

// ResolveRevision 校验 rev 在仓库里确实存在，并返回它指向的完整 commit hash。
//
// 不对分支名、远程分支、tag、commit hash 做任何特殊处理——git 自己的 revision
// 解析规则已经覆盖了全部形式，重新实现一遍只会引入与 git 不一致的边界行为。
func ResolveRevision(ctx context.Context, repoDir, rev string) (string, error) {
	out, err := git(ctx, repoDir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve revision %q: not a commit in this repository; "+
			"use an existing branch (e.g. dev), remote branch (e.g. origin/dev), tag, or commit hash", rev)
	}
	hash := strings.TrimSpace(string(out))
	if hash == "" {
		return "", fmt.Errorf("resolve revision %q: not a commit in this repository; "+
			"use an existing branch (e.g. dev), remote branch (e.g. origin/dev), tag, or commit hash", rev)
	}
	return hash, nil
}

// WorkTreeDirtyPaths 返回工作区与索引中所有偏离 HEAD 的路径（含未跟踪文件），
// 干净时返回空切片。
//
// 基于 base 的审查必须在干净的工作区上跑：diff 是按 HEAD 这个提交算出来的，
// 而模型用 read_file 读到的是工作区里的文件。工作区一旦有未提交的改动，这两者
// 就对不上，模型会拿着一份和 diff 不一致的代码下判断。
func WorkTreeDirtyPaths(ctx context.Context, repoDir string) ([]string, error) {
	out, err := git(ctx, repoDir, "status", "--porcelain", "-z", "--untracked-files=normal")
	if err != nil {
		return nil, fmt.Errorf("check work tree status: %w", err)
	}

	var paths []string
	for _, entry := range bytes.Split(out, []byte{0}) {
		// porcelain 的每条记录形如 "XY <path>"，前两位是索引/工作区状态码。
		// 短于 4 字节的只可能是尾部空串，不是记录。
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, string(bytes.TrimLeft(entry[2:], " ")))
	}
	return paths, nil
}

// ErrMergeTreeUnsupported 表示当前 git 版本没有只读的 `merge-tree --write-tree`
// （2.38 之前的形式不同，输出也无法可靠解析）。调用方应当据此跳过冲突检测，
// 而不是让审查失败——git 版本旧不该让整个功能不可用。
var ErrMergeTreeUnsupported = errors.New("git merge-tree --write-tree is unavailable (requires git 2.38+)")

// MergeConflicts 返回把 HEAD 合并到 base 时会冲突的路径，无冲突时返回空切片。
//
// 用 `git merge-tree --write-tree`：它只往对象库里写树对象，不碰工作区也不碰
// 索引，因此可以在审查开始前安全地做一次预检。合不上的分支没有审查价值——
// 冲突意味着 diff 本身还不是最终要合进 base 的内容。
//
// git 2.38 之前的 `merge-tree` 是完全不同的三参数形式，这里不去兼容，直接返回
// ErrMergeTreeUnsupported 让调用方降级。
func MergeConflicts(ctx context.Context, repoDir, base string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--name-only", base, "HEAD")
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return nil, nil // 退出码 0：可以干净合并
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return nil, fmt.Errorf("check merge conflicts against %q: %w", base, err)
	}
	// 退出码 1 表示有冲突，其余（128 等）是 git 自身的用法/环境错误——老版本
	// 不认识 --write-tree 就落在这里。
	if exitErr.ExitCode() != 1 {
		return nil, ErrMergeTreeUnsupported
	}

	// --name-only 下的输出是：
	//   <tree OID>
	//   <每行一个冲突路径>
	//   <空行>
	//   <人类可读的 "CONFLICT (content): ..." 说明>
	// 跳过首行的 OID，读到空行为止——空行之后是给人看的说明文字，
	// 继续读下去会把它当成文件名收进来。
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	var conflicts []string
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		conflicts = append(conflicts, line)
	}
	return conflicts, nil
}
