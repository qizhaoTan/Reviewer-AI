# Reviewer-AI Development Guide

## Product scope

Reviewer-AI is a Go CLI for reviewing Git staged changes. The first milestone only reads the index (`git diff --cached`) and emits deterministic JSON; LLM integration comes later.

## Engineering rules

- Keep Git input collection deterministic and separate from LLM reasoning.
- Add tests for every input-boundary behavior, especially staged versus unstaged changes.
- Write all Go unit tests in table-driven style with named subtests, including single-case tests so new cases can be added without restructuring.
- Prefer the Go standard library until an external dependency has a demonstrated need.
- Run `go test ./...` and `gofmt` before handing off changes.
- When adding a `CLAUDE.md` in a subdirectory, create `AGENTS.md` there as a relative symlink to it.
