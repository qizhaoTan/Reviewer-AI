package tool

import (
	"context"
	"encoding/json"
	"testing"
)

// TestToolImplementations 确认每个具体工具的 Definition().Name 符合预期，
// 这是查找该工具（FindToolByName）时唯一使用的名称来源。
func TestToolImplementations(t *testing.T) {
	tests := []struct {
		name string
		tool ITool
	}{
		{name: "read_file", tool: ReadFileTool{}},
		{name: "glob", tool: GlobTool{}},
		{name: "grep", tool: GrepTool{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if def := tt.tool.Definition(); def.Name != tt.name {
				t.Errorf("Definition().Name = %q, want %q", def.Name, tt.name)
			}
		})
	}
}

// TestReadFileToolExecute 验证 ReadFileTool.Execute 委托给了 ReadFile，行为一致。
func TestReadFileToolExecute(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/a.go", "package a\n")

	args, err := json.Marshal(ReadFileInput{Path: "a.go"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	want := ReadFile(context.Background(), root, args)
	got := ReadFileTool{}.Execute(context.Background(), root, args)
	if got != want {
		t.Errorf("ReadFileTool.Execute() = %+v, want %+v (ReadFile())", got, want)
	}
}

// TestGlobToolExecute 验证 GlobTool.Execute 委托给了 Glob，行为一致。
func TestGlobToolExecute(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/a.go", "package a\n")

	args, err := json.Marshal(GlobInput{Pattern: "*.go"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	want := Glob(context.Background(), root, args)
	got := GlobTool{}.Execute(context.Background(), root, args)
	if got != want {
		t.Errorf("GlobTool.Execute() = %+v, want %+v (Glob())", got, want)
	}
}

// TestGrepToolExecute 验证 GrepTool.Execute 委托给了 Grep，行为一致。
func TestGrepToolExecute(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/a.go", "package a\n")

	args, err := json.Marshal(GrepInput{Pattern: "package"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	want := Grep(context.Background(), root, args)
	got := GrepTool{}.Execute(context.Background(), root, args)
	if got != want {
		t.Errorf("GrepTool.Execute() = %+v, want %+v (Grep())", got, want)
	}
}

func TestFindToolByName(t *testing.T) {
	tools := []ITool{ReadFileTool{}, GlobTool{}, GrepTool{}}

	tests := []struct {
		name     string
		lookup   string
		wantTool ITool
		wantErr  bool
	}{
		{name: "first tool", lookup: "read_file", wantTool: ReadFileTool{}},
		{name: "middle tool", lookup: "glob", wantTool: GlobTool{}},
		{name: "last tool", lookup: "grep", wantTool: GrepTool{}},
		{name: "unknown tool", lookup: "bash", wantErr: true},
		{name: "empty name", lookup: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FindToolByName(tools, tt.lookup)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("FindToolByName(%q) error = nil, want error", tt.lookup)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindToolByName(%q) unexpected error: %v", tt.lookup, err)
			}
			if got != tt.wantTool {
				t.Errorf("FindToolByName(%q) = %v, want %v", tt.lookup, got, tt.wantTool)
			}
		})
	}
}
