package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
)

type output struct {
	SelectedCount int              `json:"selected_count"`
	Changes       []gitdiff.Change `json:"changes"`
}

func main() {
	changes, err := gitdiff.LoadStaged(context.Background(), ".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "reviewer-ai: %v\n", err)
		os.Exit(1)
	}

	result := output{
		SelectedCount: len(changes),
		Changes:       changes,
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "reviewer-ai: encode output: %v\n", err)
		os.Exit(1)
	}
}
