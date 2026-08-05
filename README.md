# Reviewer-AI

Reviewer-AI is a Go CLI that reviews staged Git changes before they are committed.

## Current milestone

The first vertical slice reads only the Git index and emits a deterministic JSON inventory. LLM review and precise comment location will be added in later milestones.

```bash
go run ./cmd/reviewer-ai
```

The command intentionally uses `git diff --cached`, so unstaged working-tree changes are excluded.
