package gitdiff

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffIgnore(t *testing.T) {
	for _, mode := range []string{"staged", "base"} {
		t.Run(mode, func(t *testing.T) {
			for _, rules := range [][]string{{"*.log", "logs/**"}, {"**"}, nil} {
				t.Run(strings.Join(rules, ","), func(t *testing.T) {
					repo := newRepo(t)
					writeFile(t, filepath.Join(repo, "removed.log"), "old log\n")
					writeFile(t, filepath.Join(repo, "app.log"), "old log\n")
					runGit(t, repo, "add", "-A")
					runGit(t, repo, "commit", "-m", "base")
					runGit(t, repo, "branch", "ignore-base")
					writeFile(t, filepath.Join(repo, "keep.go"), "staged code\n")
					writeFile(t, filepath.Join(repo, "app.log"), "new log\n")
					writeFile(t, filepath.Join(repo, "new.log"), "new log\n")
					if err := os.MkdirAll(filepath.Join(repo, "logs"), 0700); err != nil {
						t.Fatal(err)
					}
					writeFile(t, filepath.Join(repo, "logs", "data.txt"), "log data\n")
					if err := os.Remove(filepath.Join(repo, "removed.log")); err != nil {
						t.Fatal(err)
					}
					runGit(t, repo, "add", "-A")
					var changes []Change
					var err error
					if mode == "base" {
						runGit(t, repo, "commit", "-m", "changes")
						changes, err = LoadDiffRange(context.Background(), repo, "ignore-base", rules...)
					} else {
						writeFile(t, filepath.Join(repo, "keep.go"), "unstaged code\n")
						changes, err = LoadStaged(context.Background(), repo, rules...)
					}
					if err != nil {
						t.Fatal(err)
					}
					want := 1
					if len(rules) == 0 {
						want = 5
					}
					if len(rules) == 1 {
						want = 0
					}
					if len(changes) != want {
						t.Fatalf("got %d changes want %d: %+v", len(changes), want, changes)
					}
					if want == 1 && (changes[0].Path != "keep.go" || !strings.Contains(changes[0].Patch, "staged code") || strings.Contains(changes[0].Patch, "unstaged code")) {
						t.Fatalf("bad snapshot: %+v", changes)
					}
				})
			}
		})
	}
}
