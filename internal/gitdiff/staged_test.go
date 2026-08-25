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

func TestSnapshotHash(t *testing.T) {
	a := []Change{{Status: "M", Path: "a.go", Patch: "pa"}, {Status: "M", Path: "b.go", Patch: "pb"}}
	aReordered := []Change{{Status: "M", Path: "b.go", Patch: "pb"}, {Status: "M", Path: "a.go", Patch: "pa"}}
	differentPatch := []Change{{Status: "M", Path: "a.go", Patch: "pa-changed"}, {Status: "M", Path: "b.go", Patch: "pb"}}
	extraFile := []Change{{Status: "M", Path: "a.go", Patch: "pa"}, {Status: "M", Path: "b.go", Patch: "pb"}, {Status: "A", Path: "c.go", Patch: "pc"}}

	tests := []struct {
		name      string
		a, b      []Change
		wantEqual bool
	}{
		{name: "identical input produces identical hash", a: a, b: a, wantEqual: true},
		{name: "order does not affect hash", a: a, b: aReordered, wantEqual: true},
		{name: "different patch content changes hash", a: a, b: differentPatch, wantEqual: false},
		{name: "extra file changes hash", a: a, b: extraFile, wantEqual: false},
		{name: "both empty produces identical hash", a: nil, b: nil, wantEqual: true},
		{name: "empty vs non-empty differ", a: nil, b: a, wantEqual: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := SnapshotHash(tt.a)
			gotB := SnapshotHash(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("SnapshotHash(%v) == SnapshotHash(%v) = %v, want %v", tt.a, tt.b, gotA == gotB, tt.wantEqual)
			}
		})
	}
}

func TestSnapshotHashDoesNotMutateInput(t *testing.T) {
	changes := []Change{{Status: "M", Path: "b.go", Patch: "pb"}, {Status: "M", Path: "a.go", Patch: "pa"}}
	original := append([]Change(nil), changes...)

	SnapshotHash(changes)

	if len(changes) != len(original) {
		t.Fatalf("SnapshotHash mutated slice length: got %d, want %d", len(changes), len(original))
	}
	for i := range changes {
		if changes[i] != original[i] {
			t.Errorf("SnapshotHash mutated input order at index %d: got %+v, want %+v", i, changes[i], original[i])
		}
	}
}

func TestStageAll(t *testing.T) {
	type WantChange struct {
		status string
		path   string
	}
	tests := []struct {
		name string
		// arrange 返回 (repo 根, 传给 StageAll 的目录)。两者可以不同，
		// 用来验证在子目录调用时依然覆盖整棵工作树。
		arrange     func(*testing.T) (string, string)
		wantErr     bool
		wantChanges []WantChange
	}{
		{
			name: "stages new, modified and deleted files across the whole tree",
			arrange: func(t *testing.T) (string, string) {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "kept.go"), "package a\n\nconst v = \"base\"\n")
				writeFile(t, filepath.Join(repo, "gone.go"), "package a\n")
				runGit(t, repo, "add", ".")
				runGit(t, repo, "commit", "-m", "initial")

				writeFile(t, filepath.Join(repo, "kept.go"), "package a\n\nconst v = \"changed\"\n")
				if err := os.Remove(filepath.Join(repo, "gone.go")); err != nil {
					t.Fatalf("remove gone.go: %v", err)
				}
				if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
					t.Fatalf("mkdir sub: %v", err)
				}
				writeFile(t, filepath.Join(repo, "sub", "added.go"), "package sub\n")
				return repo, repo
			},
			wantChanges: []WantChange{
				{status: "D", path: "gone.go"},
				{status: "M", path: "kept.go"},
				{status: "A", path: "sub/added.go"},
			},
		},
		{
			name: "called from a subdirectory still stages the whole tree",
			arrange: func(t *testing.T) (string, string) {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "root.go"), "package a\n")
				runGit(t, repo, "add", ".")
				runGit(t, repo, "commit", "-m", "initial")

				if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
					t.Fatalf("mkdir sub: %v", err)
				}
				writeFile(t, filepath.Join(repo, "sub", "added.go"), "package sub\n")
				writeFile(t, filepath.Join(repo, "root.go"), "package a\n\nconst v = 1\n")
				return repo, filepath.Join(repo, "sub")
			},
			wantChanges: []WantChange{
				{status: "M", path: "root.go"},
				{status: "A", path: "sub/added.go"},
			},
		},
		{
			name: "clean tree stages nothing and does not error",
			arrange: func(t *testing.T) (string, string) {
				repo := newRepo(t)
				writeFile(t, filepath.Join(repo, "a.go"), "package a\n")
				runGit(t, repo, "add", ".")
				runGit(t, repo, "commit", "-m", "initial")
				return repo, repo
			},
			wantChanges: nil,
		},
		{
			name: "not a git repository reports an error",
			arrange: func(t *testing.T) (string, string) {
				dir := t.TempDir()
				return dir, dir
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, dir := tt.arrange(t)

			err := StageAll(context.Background(), dir)
			if tt.wantErr {
				if err == nil {
					t.Fatal("StageAll() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("StageAll() error = %v", err)
			}

			changes, err := LoadStaged(context.Background(), repo)
			if err != nil {
				t.Fatalf("LoadStaged() error = %v", err)
			}
			if len(changes) != len(tt.wantChanges) {
				t.Fatalf("LoadStaged() returned %d changes, want %d: %+v", len(changes), len(tt.wantChanges), changes)
			}
			for i, want := range tt.wantChanges {
				if changes[i].Status != want.status || changes[i].Path != want.path {
					t.Fatalf("change[%d] = (%s, %s), want (%s, %s)", i, changes[i].Status, changes[i].Path, want.status, want.path)
				}
			}
		})
	}
}
