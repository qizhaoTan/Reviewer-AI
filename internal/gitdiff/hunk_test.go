package gitdiff

import "testing"

func TestParseHunks(t *testing.T) {
	type wantLine struct {
		typ     HunkLineType
		content string
	}
	type wantHunk struct {
		oldStart, oldCount int
		newStart, newCount int
		lines              []wantLine
	}

	tests := []struct {
		name  string
		patch string
		want  []wantHunk
	}{
		{
			name: "parses a single hunk skipping file level headers",
			patch: `diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -10,3 +10,4 @@ package a
 keep
-drop
+add
 tail`,
			want: []wantHunk{{
				oldStart: 10, oldCount: 3, newStart: 10, newCount: 4,
				lines: []wantLine{
					{HunkContext, "keep"},
					{HunkDeleted, "drop"},
					{HunkAdded, "add"},
					{HunkContext, "tail"},
				},
			}},
		},
		{
			name: "parses multiple hunks in one file",
			patch: `--- a/a.go
+++ b/a.go
@@ -1,2 +1,2 @@
-one
+uno
@@ -20,2 +20,3 @@
 ctx
+new`,
			want: []wantHunk{
				{
					oldStart: 1, oldCount: 2, newStart: 1, newCount: 2,
					lines: []wantLine{{HunkDeleted, "one"}, {HunkAdded, "uno"}},
				},
				{
					oldStart: 20, oldCount: 2, newStart: 20, newCount: 3,
					lines: []wantLine{{HunkContext, "ctx"}, {HunkAdded, "new"}},
				},
			},
		},
		{
			name: "header without explicit counts defaults to one line",
			patch: `@@ -5 +5 @@
-a
+b`,
			want: []wantHunk{{
				oldStart: 5, oldCount: 1, newStart: 5, newCount: 1,
				lines: []wantLine{{HunkDeleted, "a"}, {HunkAdded, "b"}},
			}},
		},
		{
			name: "no newline marker is not counted as a content line",
			patch: `@@ -1,1 +1,1 @@
-old
\ No newline at end of file
+new`,
			want: []wantHunk{{
				oldStart: 1, oldCount: 1, newStart: 1, newCount: 1,
				lines: []wantLine{{HunkDeleted, "old"}, {HunkAdded, "new"}},
			}},
		},
		{
			name: "stops at the next file diff header",
			patch: `@@ -1,1 +1,1 @@
+mine
diff --git a/b.go b/b.go
@@ -9,1 +9,1 @@
+theirs`,
			want: []wantHunk{{
				oldStart: 1, oldCount: 1, newStart: 1, newCount: 1,
				lines: []wantLine{{HunkAdded, "mine"}},
			}},
		},
		{
			name: "an empty context line keeps its empty content",
			patch: `@@ -1,3 +1,3 @@
 a

 b`,
			want: []wantHunk{{
				oldStart: 1, oldCount: 3, newStart: 1, newCount: 3,
				lines: []wantLine{{HunkContext, "a"}, {HunkContext, ""}, {HunkContext, "b"}},
			}},
		},
		{name: "patch with no hunks yields nothing", patch: "Binary files a/x.bin and b/x.bin differ", want: nil},
		{name: "empty patch yields nothing", patch: "", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseHunks(tt.patch)
			if len(got) != len(tt.want) {
				t.Fatalf("len(ParseHunks()) = %d, want %d (%+v)", len(got), len(tt.want), got)
			}
			for i, want := range tt.want {
				h := got[i]
				if h.OldStart != want.oldStart || h.OldCount != want.oldCount ||
					h.NewStart != want.newStart || h.NewCount != want.newCount {
					t.Errorf("hunk[%d] header = -%d,%d +%d,%d, want -%d,%d +%d,%d",
						i, h.OldStart, h.OldCount, h.NewStart, h.NewCount,
						want.oldStart, want.oldCount, want.newStart, want.newCount)
				}
				if len(h.Lines) != len(want.lines) {
					t.Fatalf("hunk[%d] has %d lines, want %d (%+v)", i, len(h.Lines), len(want.lines), h.Lines)
				}
				for j, wantLine := range want.lines {
					if h.Lines[j].Type != wantLine.typ || h.Lines[j].Content != wantLine.content {
						t.Errorf("hunk[%d].Lines[%d] = {%v, %q}, want {%v, %q}",
							i, j, h.Lines[j].Type, h.Lines[j].Content, wantLine.typ, wantLine.content)
					}
				}
			}
		})
	}
}
