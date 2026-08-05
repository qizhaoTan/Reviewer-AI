package gitdiff

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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
