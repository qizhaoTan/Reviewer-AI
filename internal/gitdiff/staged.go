package gitdiff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Change is one file-level change frozen in the Git index.
type Change struct {
	Status string `json:"status"`
	Path   string `json:"path"`
	Patch  string `json:"patch"`
}

// LoadStaged returns file-level patches from the Git index relative to HEAD.
// Working-tree-only changes are deliberately excluded.
func LoadStaged(ctx context.Context, repoDir string) ([]Change, error) {
	entries, err := git(ctx, repoDir, "diff", "--cached", "--name-status", "-z", "--no-renames", "--")
	if err != nil {
		return nil, fmt.Errorf("list staged changes: %w", err)
	}

	fields := bytes.Split(entries, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("parse staged changes: expected status/path pairs, got %d fields", len(fields))
	}

	changes := make([]Change, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		status := string(fields[i])
		path := string(fields[i+1])
		patch, err := git(ctx, repoDir,
			"diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=3", "--", path,
		)
		if err != nil {
			return nil, fmt.Errorf("read staged patch for %q: %w", path, err)
		}
		changes = append(changes, Change{Status: status, Path: path, Patch: string(patch)})
	}
	return changes, nil
}

// CurrentBranch returns the short name of the currently checked-out branch
// (e.g. "main"). In detached HEAD state it returns the short commit hash instead,
// since there is no branch name to report.
func CurrentBranch(ctx context.Context, repoDir string) (string, error) {
	out, err := git(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve current branch: %w", err)
	}
	branch := strings.TrimSpace(string(out))
	if branch == "HEAD" {
		// rev-parse --abbrev-ref reports the literal "HEAD" in detached state;
		// fall back to the short commit hash so callers still get a stable identifier.
		hashOut, err := git(ctx, repoDir, "rev-parse", "--short", "HEAD")
		if err != nil {
			return "", fmt.Errorf("resolve detached HEAD commit: %w", err)
		}
		return strings.TrimSpace(string(hashOut)), nil
	}
	return branch, nil
}

// SnapshotHash returns a deterministic content hash of changes, independent of
// slice order. Two snapshots with the same set of (status, path, patch) triples
// always hash to the same value, even if LoadStaged happened to return them in a
// different order. Used to look up a prior run whose staged content exactly
// matches the current one (see store.LoadRunByHash), so re-reviewing content
// that was already reviewed (e.g. after a git stash / stash pop round trip)
// can reuse the earlier result instead of starting over.
func SnapshotHash(changes []Change) string {
	sorted := make([]Change, len(changes))
	copy(sorted, changes)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	h := sha256.New()
	for _, c := range sorted {
		h.Write([]byte(c.Status))
		h.Write([]byte{0})
		h.Write([]byte(c.Path))
		h.Write([]byte{0})
		h.Write([]byte(c.Patch))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func git(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("git %v: %s: %w", args, bytes.TrimSpace(stderr.Bytes()), err)
		}
		return nil, fmt.Errorf("git %v: %w", args, err)
	}
	return out, nil
}

// StageAll stages every change in the repository, equivalent to running
// `git add -A` at the repository root. Unlike `git add .`, which is relative to
// the current directory, this always covers the whole work tree regardless of
// where the command was invoked from — that is what callers mean by "stage
// everything before reviewing".
func StageAll(ctx context.Context, repoDir string) error {
	if _, err := git(ctx, repoDir, "add", "-A", "--"); err != nil {
		return fmt.Errorf("stage all changes: %w", err)
	}
	return nil
}
