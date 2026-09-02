package gitdiff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestLoadDiffRange(t *testing.T) {
	type wantChange struct {
		status        string
		path          string
		patchContains []string
		patchExcludes []string
	}
	tests := []struct {
		name        string
		arrange     func(*testing.T) string
		base        string
		wantErr     bool
		wantChanges []wantChange
	}{
		{
			name: "reports only commits made on the feature branch",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "shared.go"), "package sample\n\nconst shared = \"base\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "-b", "feature")
				writeFile(t, filepath.Join(repo, "feature.go"), "package sample\n\nconst feature = true\n")
				runGit(t, repo, "add", "feature.go")
				runGit(t, repo, "commit", "-m", "add feature")
				return repo
			},
			base: "dev",
			wantChanges: []wantChange{
				{status: "A", path: "feature.go", patchContains: []string{"const feature = true"}},
			},
		},
		{
			name: "excludes commits added to base after the branch point",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "shared.go"), "package sample\n\nconst shared = \"base\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "feature")

				// dev 上后来的提交：三点 diff 不应把它算成本分支的改动。
				writeFile(t, filepath.Join(repo, "dev_only.go"), "package sample\n\nconst devOnly = true\n")
				runGit(t, repo, "add", "dev_only.go")
				runGit(t, repo, "commit", "-m", "dev moves on")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "feature")
				writeFile(t, filepath.Join(repo, "feature.go"), "package sample\n\nconst feature = true\n")
				runGit(t, repo, "add", "feature.go")
				runGit(t, repo, "commit", "-m", "add feature")
				return repo
			},
			base: "dev",
			wantChanges: []wantChange{
				{status: "A", path: "feature.go", patchContains: []string{"const feature = true"}},
			},
		},
		{
			name: "collapses multiple commits on the branch into one patch per file",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				path := filepath.Join(repo, "counter.go")
				writeFile(t, path, "package sample\n\nconst count = 0\n")
				runGit(t, repo, "add", "counter.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "-b", "feature")
				writeFile(t, path, "package sample\n\nconst count = 1\n")
				runGit(t, repo, "add", "counter.go")
				runGit(t, repo, "commit", "-m", "bump to 1")
				writeFile(t, path, "package sample\n\nconst count = 2\n")
				runGit(t, repo, "add", "counter.go")
				runGit(t, repo, "commit", "-m", "bump to 2")
				return repo
			},
			base: "dev",
			wantChanges: []wantChange{
				{
					status:        "M",
					path:          "counter.go",
					patchContains: []string{"-const count = 0", "+const count = 2"},
					// 中间态只存在于分支内部的提交里，squash 之后不该出现。
					patchExcludes: []string{"const count = 1"},
				},
			},
		},
		{
			name: "ignores uncommitted work tree changes",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				path := filepath.Join(repo, "app.go")
				writeFile(t, path, "package sample\n\nconst value = \"base\"\n")
				runGit(t, repo, "add", "app.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "-b", "feature")
				writeFile(t, path, "package sample\n\nconst value = \"committed\"\n")
				runGit(t, repo, "add", "app.go")
				runGit(t, repo, "commit", "-m", "commit change")
				writeFile(t, path, "package sample\n\nconst value = \"uncommitted\"\n")
				return repo
			},
			base: "dev",
			wantChanges: []wantChange{
				{
					status:        "M",
					path:          "app.go",
					patchContains: []string{`const value = "committed"`},
					patchExcludes: []string{`const value = "uncommitted"`},
				},
			},
		},
		{
			name: "reports deletions made on the branch",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				path := filepath.Join(repo, "gone.go")
				writeFile(t, path, "package sample\n\nconst gone = true\n")
				runGit(t, repo, "add", "gone.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "-b", "feature")
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove %s: %v", path, err)
				}
				runGit(t, repo, "add", "-A")
				runGit(t, repo, "commit", "-m", "delete file")
				return repo
			},
			base: "dev",
			wantChanges: []wantChange{
				{status: "D", path: "gone.go", patchContains: []string{"-const gone = true"}},
			},
		},
		{
			name: "branch identical to base yields nothing",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "a.go"), "package sample\n")
				runGit(t, repo, "add", "a.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "dev")
				return repo
			},
			base:        "dev",
			wantChanges: nil,
		},
		{
			name: "unknown base is an error",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "a.go"), "package sample\n")
				runGit(t, repo, "add", "a.go")
				runGit(t, repo, "commit", "-m", "initial")
				return repo
			},
			base:    "no-such-branch",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := tt.arrange(t)
			got, err := LoadDiffRange(context.Background(), repo, tt.base)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LoadDiffRange() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadDiffRange() error = %v", err)
			}
			if len(got) != len(tt.wantChanges) {
				t.Fatalf("LoadDiffRange() returned %d changes, want %d (%+v)", len(got), len(tt.wantChanges), got)
			}
			for i, want := range tt.wantChanges {
				if got[i].Status != want.status || got[i].Path != want.path {
					t.Errorf("change[%d] = {%s %s}, want {%s %s}", i, got[i].Status, got[i].Path, want.status, want.path)
				}
				for _, fragment := range want.patchContains {
					if !strings.Contains(got[i].Patch, fragment) {
						t.Errorf("change[%d].Patch does not contain %q:\n%s", i, fragment, got[i].Patch)
					}
				}
				for _, fragment := range want.patchExcludes {
					if strings.Contains(got[i].Patch, fragment) {
						t.Errorf("change[%d].Patch unexpectedly contains %q:\n%s", i, fragment, got[i].Patch)
					}
				}
			}
		})
	}
}

func TestResolveRevision(t *testing.T) {
	tests := []struct {
		name    string
		rev     func(*testing.T, string) string
		wantErr bool
	}{
		{name: "resolves a local branch", rev: func(*testing.T, string) string { return "dev" }},
		{name: "resolves a tag", rev: func(*testing.T, string) string { return "v1" }},
		{name: "resolves HEAD", rev: func(*testing.T, string) string { return "HEAD" }},
		{name: "rejects an unknown revision", rev: func(*testing.T, string) string { return "nope" }, wantErr: true},
		{name: "rejects an empty revision", rev: func(*testing.T, string) string { return "" }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			writeFile(t, filepath.Join(repo, "a.go"), "package sample\n")
			runGit(t, repo, "add", "a.go")
			runGit(t, repo, "commit", "-m", "initial")
			runGit(t, repo, "branch", "dev")
			runGit(t, repo, "tag", "v1")

			got, err := ResolveRevision(context.Background(), repo, tt.rev(t, repo))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveRevision() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRevision() error = %v", err)
			}
			if len(got) != 40 {
				t.Errorf("ResolveRevision() = %q, want a 40-character commit hash", got)
			}
		})
	}
}

func TestWorkTreeDirtyPaths(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*testing.T, string)
		want    []string
	}{
		{
			name:    "clean work tree reports nothing",
			arrange: func(*testing.T, string) {},
			want:    nil,
		},
		{
			name: "unstaged modification is reported",
			arrange: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "a.go"), "package sample\n\nconst changed = true\n")
			},
			want: []string{"a.go"},
		},
		{
			name: "staged modification is reported",
			arrange: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "a.go"), "package sample\n\nconst staged = true\n")
				runGit(t, repo, "add", "a.go")
			},
			want: []string{"a.go"},
		},
		{
			name: "untracked file is reported",
			arrange: func(t *testing.T, repo string) {
				writeFile(t, filepath.Join(repo, "new.go"), "package sample\n")
			},
			want: []string{"new.go"},
		},
		{
			name: "deleted file is reported",
			arrange: func(t *testing.T, repo string) {
				if err := os.Remove(filepath.Join(repo, "a.go")); err != nil {
					t.Fatalf("remove a.go: %v", err)
				}
			},
			want: []string{"a.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newRepo(t)
			writeFile(t, filepath.Join(repo, "a.go"), "package sample\n")
			runGit(t, repo, "add", "a.go")
			runGit(t, repo, "commit", "-m", "initial")
			tt.arrange(t, repo)

			got, err := WorkTreeDirtyPaths(context.Background(), repo)
			if err != nil {
				t.Fatalf("WorkTreeDirtyPaths() error = %v", err)
			}
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("WorkTreeDirtyPaths() = %v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i] != want {
					t.Errorf("WorkTreeDirtyPaths()[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

func TestMergeConflicts(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*testing.T) string
		want    []string
	}{
		{
			name: "disjoint changes merge cleanly",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "shared.go"), "package sample\n\nconst shared = \"base\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "feature")

				writeFile(t, filepath.Join(repo, "dev.go"), "package sample\n\nconst dev = true\n")
				runGit(t, repo, "add", "dev.go")
				runGit(t, repo, "commit", "-m", "dev side")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "feature")
				writeFile(t, filepath.Join(repo, "feature.go"), "package sample\n\nconst feature = true\n")
				runGit(t, repo, "add", "feature.go")
				runGit(t, repo, "commit", "-m", "feature side")
				return repo
			},
			want: nil,
		},
		{
			name: "both sides editing the same lines conflict",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				path := filepath.Join(repo, "shared.go")
				writeFile(t, path, "package sample\n\nconst shared = \"base\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "initial")
				runGit(t, repo, "branch", "feature")

				writeFile(t, path, "package sample\n\nconst shared = \"dev\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "dev edits shared")
				runGit(t, repo, "branch", "dev")

				runGit(t, repo, "checkout", "--quiet", "feature")
				writeFile(t, path, "package sample\n\nconst shared = \"feature\"\n")
				runGit(t, repo, "add", "shared.go")
				runGit(t, repo, "commit", "-m", "feature edits shared")
				return repo
			},
			want: []string{"shared.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := tt.arrange(t)
			got, err := MergeConflicts(context.Background(), repo, "dev")
			if errors.Is(err, ErrMergeTreeUnsupported) {
				t.Skip("git is older than 2.38; merge-tree --write-tree unavailable")
			}
			if err != nil {
				t.Fatalf("MergeConflicts() error = %v", err)
			}
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("MergeConflicts() = %v, want %v", got, tt.want)
			}
			for i, want := range tt.want {
				if got[i] != want {
					t.Errorf("MergeConflicts()[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}
