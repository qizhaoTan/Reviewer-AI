package review

import (
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
)

// samplePatch 是一份真实形状的 unified diff，用于验证行号推算。
// 新文件侧的行号推演（供测试断言参考）：
//
//	10  func Load(path string) (*File, error) {   （上下文）
//	11  	data, err := os.ReadFile(path)         （上下文）
//	12  	if err != nil {                        （新增）
//	13  		return nil, err                    （新增）
//	14  	}                                      （新增）
//	15  	var f File                             （上下文）
//	16  	return &f, nil                         （上下文）
const samplePatch = `diff --git a/config.go b/config.go
index 1234567..89abcde 100644
--- a/config.go
+++ b/config.go
@@ -10,5 +10,8 @@ package config
 func Load(path string) (*File, error) {
 	data, err := os.ReadFile(path)
+	if err != nil {
+		return nil, err
+	}
 	var f File
 	return &f, nil
`

// blankLinePatch 的**代码本身**含空行，用来盯住 anchor 侧与 diff 侧的对称性。
// samplePatch 里一行空行都没有，所以它只能验证"anchor 带空行"这一半；真实代码
// 里空行随处可见，diff 侧带空行才是常态。
//
// 新文件侧的行号推演：
//
//	30  func Draw(id int) error {     （上下文）
//	31  	base := find(id)           （新增）
//	32                                （新增，空行）
//	33  	if base == nil {           （新增）
//	34  		return errNotFound     （新增）
//	35  	}                          （新增）
//	36                                （新增，空行）
//	37  	return grant(base)         （新增）
//	38  }                             （上下文）
const blankLinePatch = `diff --git a/draw.go b/draw.go
index 1234567..89abcde 100644
--- a/draw.go
+++ b/draw.go
@@ -30,2 +30,9 @@ package draw
 func Draw(id int) error {
+	base := find(id)
+
+	if base == nil {
+		return errNotFound
+	}
+
+	return grant(base)
 }
`

// twoHunkPatch / twoHunkFile 演示"一个文件被切成两个 hunk"：两处改动隔得远，
// 中间那段没改的代码超出了 3 行上下文，因此完全不在 diff 里。
//
// twoHunkFile 的行号：1 start() / 2 first() / 3-6 pad1..pad4 / 7 last() / 8 end()
// 两个 hunk 分别覆盖 1-2 行与 7-8 行，中间的 pad2/pad3 两行两侧都没有。
const twoHunkPatch = `diff --git a/split.go b/split.go
index 1234567..89abcde 100644
--- a/split.go
+++ b/split.go
@@ -1,1 +1,2 @@ package split
 start()
+	first()
@@ -6,1 +7,2 @@ package split
+	last()
 end()
`

const twoHunkFile = `start()
	first()
	pad1()
	pad2()
	pad3()
	pad4()
	last()
end()
`

// deletionPatch 只删不增，用于验证"意见针对被删掉的代码"时退到旧文件侧定位。
// 旧文件侧行号：20 = "func Deprecated() {"，21 = "	panic(\"unused\")"，22 = "}"
const deletionPatch = `--- a/old.go
+++ b/old.go
@@ -20,3 +20,0 @@ package old
-func Deprecated() {
-	panic("unused")
-}
`

func TestResolveAnchor(t *testing.T) {
	tests := []struct {
		name      string
		patch     string
		anchor    string
		wantStart int
		wantEnd   int
		wantFound bool
	}{
		{
			name:      "single added line resolves to its new file line",
			patch:     samplePatch,
			anchor:    "return nil, err",
			wantStart: 13, wantEnd: 13, wantFound: true,
		},
		{
			name:      "multi line anchor spans a range",
			patch:     samplePatch,
			anchor:    "if err != nil {\n\treturn nil, err\n}",
			wantStart: 12, wantEnd: 14, wantFound: true,
		},
		{
			name:      "anchor copied straight from the diff with plus markers still matches",
			patch:     samplePatch,
			anchor:    "+	if err != nil {\n+		return nil, err\n+	}",
			wantStart: 12, wantEnd: 14, wantFound: true,
		},
		{
			name:      "indentation differences are ignored",
			patch:     samplePatch,
			anchor:    "        if err != nil {\n                return nil, err\n        }",
			wantStart: 12, wantEnd: 14, wantFound: true,
		},
		{
			name:      "blank lines inside the anchor are ignored",
			patch:     samplePatch,
			anchor:    "if err != nil {\n\n\treturn nil, err\n\n}",
			wantStart: 12, wantEnd: 14, wantFound: true,
		},
		{
			// 模型通常从 read_file 的输出抄 anchor，那里没有 diff 的空行；
			// 而 diff 侧原样保留着空行。两侧形状不一致时窗口会在空行处断掉。
			name:      "blank lines in the diff are ignored when the anchor has none",
			patch:     blankLinePatch,
			anchor:    "base := find(id)\nif base == nil {",
			wantStart: 31, wantEnd: 33, wantFound: true,
		},
		{
			name:      "blank lines on both sides are ignored",
			patch:     blankLinePatch,
			anchor:    "base := find(id)\n\nif base == nil {\n\treturn errNotFound\n}",
			wantStart: 31, wantEnd: 35, wantFound: true,
		},
		{
			// 空行不进匹配序列，但行号计数器必须照常推进：这里 return grant(base)
			// 之前隔着两个空行，行号错的话会落在 35 而不是 37。
			name:      "line numbers stay correct across skipped blank lines",
			patch:     blankLinePatch,
			anchor:    "return grant(base)",
			wantStart: 37, wantEnd: 37, wantFound: true,
		},
		{
			name:      "anchor spanning the whole blank line body resolves",
			patch:     blankLinePatch,
			anchor:    "func Draw(id int) error {\nbase := find(id)\nif base == nil {\nreturn errNotFound\n}\nreturn grant(base)\n}",
			wantStart: 30, wantEnd: 38, wantFound: true,
		},
		{
			name:      "context line resolves to its new file line",
			patch:     samplePatch,
			anchor:    "var f File",
			wantStart: 15, wantEnd: 15, wantFound: true,
		},
		{
			name:      "anchor spanning context and added lines resolves",
			patch:     samplePatch,
			anchor:    "data, err := os.ReadFile(path)\nif err != nil {",
			wantStart: 11, wantEnd: 12, wantFound: true,
		},
		{
			name:      "deleted code falls back to old file line numbers",
			patch:     deletionPatch,
			anchor:    "func Deprecated() {\npanic(\"unused\")",
			wantStart: 20, wantEnd: 21, wantFound: true,
		},
		{
			name:      "anchor absent from the patch is not found",
			patch:     samplePatch,
			anchor:    "totally unrelated code",
			wantFound: false,
		},
		{
			name:      "empty anchor is not found",
			patch:     samplePatch,
			anchor:    "",
			wantFound: false,
		},
		{
			name:      "whitespace only anchor is not found",
			patch:     samplePatch,
			anchor:    "   \n\t\n",
			wantFound: false,
		},
		{
			name:      "patch without hunks is not found",
			patch:     "diff --git a/x.go b/x.go\nBinary files differ\n",
			anchor:    "anything",
			wantFound: false,
		},
		{
			name:      "anchor longer than the hunk is not found",
			patch:     samplePatch,
			anchor:    "func Load(path string) (*File, error) {\ndata, err := os.ReadFile(path)\nif err != nil {\nreturn nil, err\n}\nvar f File\nreturn &f, nil\nextra line that does not exist",
			wantFound: false,
		},
		{
			name:      "non consecutive lines do not match",
			patch:     samplePatch,
			anchor:    "if err != nil {\nvar f File",
			wantFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStart, gotEnd, gotFound := resolveAnchor(tt.patch, tt.anchor)
			if gotFound != tt.wantFound {
				t.Fatalf("resolveAnchor() found = %v, want %v (got lines %d-%d)", gotFound, tt.wantFound, gotStart, gotEnd)
			}
			if !tt.wantFound {
				return
			}
			if gotStart != tt.wantStart || gotEnd != tt.wantEnd {
				t.Errorf("resolveAnchor() = lines %d-%d, want %d-%d", gotStart, gotEnd, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestResolveAnchors(t *testing.T) {
	changes := []gitdiff.Change{
		{Status: "M", Path: "config.go", Patch: samplePatch},
	}

	// fileContent 是 config.go 的"完整文件"，比 samplePatch 那个 hunk 多出前后
	// 各若干行——正是模型用 read_file 能看到、而 diff 里不存在的那部分。
	// 行号推演：第 1 行 package config ... 第 10 行 func Load ... 第 16 行 return &f, nil
	const fileContent = `package config

import "os"

type File struct {
	Name string
}

// Load reads the config file.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	return &f, nil
}
`
	readConfigGo := func(path string) ([]byte, bool) {
		if path != "config.go" {
			return nil, false
		}
		return []byte(fileContent), true
	}

	tests := []struct {
		name     string
		findings []Finding
		changes  []gitdiff.Change
		// readFile 为 nil 时只在 diff 内定位，等价于没有二级回退。
		readFile FileReader
		// wantLines 是每条 finding 期望的 [start, end]。
		wantLines [][2]int
	}{
		{
			// hunk 只带 3 行上下文，import/type 这些行在 diff 里根本不存在，
			// 只有读全文才定位得到。
			name: "anchor outside the hunk resolves by reading the file",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "type File struct {\n\tName string\n}"},
			},
			changes:   changes,
			readFile:  readConfigGo,
			wantLines: [][2]int{{5, 7}},
		},
		{
			name: "diff match still wins over the file fallback",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "return nil, err"},
			},
			changes:   changes,
			readFile:  readConfigGo,
			wantLines: [][2]int{{13, 13}},
		},
		{
			name: "anchor outside the hunk stays unresolved without a reader",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "type File struct {\n\tName string\n}"},
			},
			changes:   changes,
			wantLines: [][2]int{{0, 0}},
		},
		{
			// reader 拒绝读（文件在工作区被改过、或读失败）时保持 0，不猜。
			name: "reader refusing the file leaves the finding unresolved",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "type File struct {"},
			},
			changes:   changes,
			readFile:  func(string) ([]byte, bool) { return nil, false },
			wantLines: [][2]int{{0, 0}},
		},
		{
			name: "anchor absent from both diff and file stays unresolved",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "func NeverExisted() {}"},
			},
			changes:   changes,
			readFile:  readConfigGo,
			wantLines: [][2]int{{0, 0}},
		},
		{
			// 两处改动隔得远，git 把它们切成两个 hunk，中间那段没改的代码不在
			// diff 里。横跨两个 hunk 的 anchor 在任何单个 hunk 内都凑不齐，
			// 读全文才连得起来——这也是不必再单独实现"跨 hunk 拼接"的原因。
			name: "anchor spanning two hunks resolves by reading the file",
			findings: []Finding{
				{ID: "f1", File: "split.go", Anchor: "first()\npad1()\npad2()\npad3()\npad4()\nlast()"},
			},
			changes: []gitdiff.Change{{Status: "M", Path: "split.go", Patch: twoHunkPatch}},
			readFile: func(path string) ([]byte, bool) {
				if path != "split.go" {
					return nil, false
				}
				return []byte(twoHunkFile), true
			},
			wantLines: [][2]int{{2, 7}},
		},
		{
			name: "resolves matching anchors and leaves the rest at zero",
			findings: []Finding{
				{ID: "f1", File: "config.go", Anchor: "return nil, err"},
				{ID: "f2", File: "config.go", Anchor: "code that is not there"},
				{ID: "f3", File: "config.go"},
			},
			changes:   changes,
			wantLines: [][2]int{{13, 13}, {0, 0}, {0, 0}},
		},
		{
			name: "finding pointing at a file outside the changeset stays unresolved",
			findings: []Finding{
				{ID: "f1", File: "not-in-diff.go", Anchor: "return nil, err"},
			},
			changes:   changes,
			wantLines: [][2]int{{0, 0}},
		},
		{
			name:      "no findings is a no-op",
			findings:  nil,
			changes:   changes,
			wantLines: nil,
		},
		{
			name:      "no changes leaves everything unresolved",
			findings:  []Finding{{ID: "f1", File: "config.go", Anchor: "return nil, err"}},
			changes:   nil,
			wantLines: [][2]int{{0, 0}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveAnchors(tt.findings, tt.changes, tt.readFile)
			if len(got) != len(tt.wantLines) {
				t.Fatalf("len(ResolveAnchors()) = %d, want %d", len(got), len(tt.wantLines))
			}
			for i, want := range tt.wantLines {
				if got[i].StartLine != want[0] || got[i].EndLine != want[1] {
					t.Errorf("findings[%d] (%s) resolved to %d-%d, want %d-%d",
						i, got[i].ID, got[i].StartLine, got[i].EndLine, want[0], want[1])
				}
			}
		})
	}
}

func TestResolveAnchorsDoesNotMutateInput(t *testing.T) {
	changes := []gitdiff.Change{{Status: "M", Path: "config.go", Patch: samplePatch}}
	findings := []Finding{{ID: "f1", File: "config.go", Anchor: "return nil, err"}}

	got := ResolveAnchors(findings, changes, nil)

	if findings[0].StartLine != 0 || findings[0].EndLine != 0 {
		t.Errorf("input finding was mutated: StartLine/EndLine = %d/%d, want 0/0",
			findings[0].StartLine, findings[0].EndLine)
	}
	if got[0].StartLine != 13 {
		t.Errorf("returned finding StartLine = %d, want 13", got[0].StartLine)
	}
}

func TestNormalizeLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "strips surrounding whitespace", in: "  foo  ", want: "foo"},
		{name: "strips leading plus marker", in: "+\tfoo", want: "foo"},
		{name: "strips leading minus marker", in: "-\tfoo", want: "foo"},
		{name: "strips marker even when it follows leading whitespace", in: "  +foo", want: "foo"},
		{name: "strips only one marker so a leading minus in code survives", in: "+-1", want: "1"},
		{name: "blank line becomes empty", in: "   \t ", want: ""},
		{name: "plain line is unchanged", in: "return nil", want: "return nil"},
		{name: "inner whitespace is preserved", in: "  if a  ==  b {  ", want: "if a  ==  b {"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeLine(tt.in); got != tt.want {
				t.Errorf("normalizeLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
