package gitdiff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadStaged(t *testing.T) {
	type WantChange struct {
		status        string
		path          string
		patchContains []string
		patchExcludes []string
	}
	tests := []struct {
		name        string
		arrange     func(*testing.T) string
		wantErr     bool
		wantChanges []WantChange
	}{
		{
			name: "reads index instead of working tree",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				path := filepath.Join(repo, "example.go")

				writeFile(t, path, "package example\n\nconst value = \"base\"\n")
				runGit(t, repo, "add", "example.go")
				runGit(t, repo, "commit", "-m", "initial")

				writeFile(t, path, "package example\n\nconst value = \"staged\"\n")
				runGit(t, repo, "add", "example.go")
				writeFile(t, path, "package example\n\nconst value = \"unstaged\"\n")
				return repo
			},
			wantChanges: []WantChange{
				{
					status:        "M",
					path:          "example.go",
					patchContains: []string{`const value = "staged"`},
					patchExcludes: []string{`const value = "unstaged"`},
				},
			},
		},
		{
			name: "loads staged additions deletions and modifications",
			arrange: func(t *testing.T) string {
				repo := newRepo(t)
				deletesPath := filepath.Join(repo, "deletes.go")
				modifiesPath := filepath.Join(repo, "modifies.go")
				writeFile(t, deletesPath, "package sample\n\nconst deleted = true\n")
				writeFile(t, modifiesPath, "package sample\n\nconst modified = \"before\"\n")
				runGit(t, repo, "add", "deletes.go", "modifies.go")
				runGit(t, repo, "commit", "-m", "initial")

				addsPath := filepath.Join(repo, "adds.go")
				writeFile(t, addsPath, "package sample\n\nconst added = true\n")
				writeFile(t, modifiesPath, "package sample\n\nconst modified = \"after\"\n")
				if err := os.Remove(deletesPath); err != nil {
					t.Fatalf("remove %s: %v", deletesPath, err)
				}
				runGit(t, repo, "add", "--all")

				return repo
			},
			wantChanges: []WantChange{
				{
					status:        "A",
					path:          "adds.go",
					patchContains: []string{"+++ b/adds.go", "+const added = true"},
				},
				{
					status:        "D",
					path:          "deletes.go",
					patchContains: []string{"+++ /dev/null", "-const deleted = true"},
				},
				{
					status:        "M",
					path:          "modifies.go",
					patchContains: []string{`-const modified = "before"`, `+const modified = "after"`},
				},
			},
		},
		{
			name: "returns no changes for clean index",
			arrange: func(t *testing.T) string {
				return newRepo(t)
			},
		},
		{
			name: "rejects non repository",
			arrange: func(t *testing.T) string {
				return t.TempDir()
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := tt.arrange(t)
			changes, err := LoadStaged(context.Background(), repo)
			if tt.wantErr {
				if err == nil {
					t.Fatal("LoadStaged() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadStaged() error = %v", err)
			}
			if len(changes) != len(tt.wantChanges) {
				t.Fatalf("len(changes) = %d, want %d", len(changes), len(tt.wantChanges))
			}
			if len(tt.wantChanges) == 0 {
				return
			}

			changesByPath := make(map[string]Change, len(changes))
			for _, change := range changes {
				changesByPath[change.Path] = change
			}
			for _, wantChange := range tt.wantChanges {
				change, ok := changesByPath[wantChange.path]
				if !ok {
					t.Errorf("change for path %q not found", wantChange.path)
					continue
				}
				if change.Status != wantChange.status || change.Path != wantChange.path {
					t.Fatalf("change = %#v, want status %q and path %q", change, wantChange.status, wantChange.path)
				}
				for _, want := range wantChange.patchContains {
					if !strings.Contains(change.Patch, want) {
						t.Errorf("patch does not contain %q:\n%s", want, change.Patch)
					}
				}
				for _, unwanted := range wantChange.patchExcludes {
					if strings.Contains(change.Patch, unwanted) {
						t.Errorf("patch contains excluded text %q:\n%s", unwanted, change.Patch)
					}
				}
			}
		})
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.email", "reviewer-ai@example.test")
	runGit(t, repo, "config", "user.name", "Reviewer AI Test")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
