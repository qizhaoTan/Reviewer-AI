package review

import (
	"reflect"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/gitdiff"
	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

// ch 是构造测试用 Change 的简写，让下面的表格能一眼看出每个用例在测什么，
// 而不是被三个字段的字面量淹没。
func ch(status, path, patch string) gitdiff.Change {
	return gitdiff.Change{Status: status, Path: path, Patch: patch}
}

// paths 把一组 Change 抽成路径列表，供断言使用——比较路径就够了，
// 全字段比较会让失败信息长到看不清哪里不一样。
func paths(changes []gitdiff.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

func TestDiffSnapshot(t *testing.T) {
	tests := []struct {
		name          string
		old           []gitdiff.Change
		new           []gitdiff.Change
		wantUnchanged []string
		wantChanged   []string
		wantVanished  []string
	}{
		{
			name:          "first review has no baseline so everything needs reviewing",
			old:           nil,
			new:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("A", "b.go", "p2")},
			wantUnchanged: []string{},
			wantChanged:   []string{"a.go", "b.go"},
			wantVanished:  []string{},
		},
		{
			name:          "identical snapshots reuse everything",
			old:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "b.go", "p2")},
			new:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "b.go", "p2")},
			wantUnchanged: []string{"a.go", "b.go"},
			wantChanged:   []string{},
			wantVanished:  []string{},
		},
		{
			// 增量重审的主场景：用户按意见改了 a.go 重新 stage，b.go 没动。
			name:          "only the edited file needs re-reviewing",
			old:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "b.go", "p2")},
			new:           []gitdiff.Change{ch("M", "a.go", "p1-fixed"), ch("M", "b.go", "p2")},
			wantUnchanged: []string{"b.go"},
			wantChanged:   []string{"a.go"},
			wantVanished:  []string{},
		},
		{
			name:          "a newly staged file is always changed",
			old:           []gitdiff.Change{ch("M", "a.go", "p1")},
			new:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("A", "new.go", "p9")},
			wantUnchanged: []string{"a.go"},
			wantChanged:   []string{"new.go"},
			wantVanished:  []string{},
		},
		{
			// 上次审过、这次不在暂存区了（已提交 / unstage）。它既不在
			// Unchanged 也不在 Changed，只出现在 Vanished。
			name:          "a file no longer staged is vanished not unchanged",
			old:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "gone.go", "p2")},
			new:           []gitdiff.Change{ch("M", "a.go", "p1")},
			wantUnchanged: []string{"a.go"},
			wantChanged:   []string{},
			wantVanished:  []string{"gone.go"},
		},
		{
			// 文件被删除并 stage：路径仍在 new 里（status D），patch 是删除
			// 全文的那份，与上次的 M patch 不同，所以要重审——重审一个删除
			// 是有意义的（比如"这个函数还有别处在调用"）。
			name:          "a staged deletion is a changed file not a vanished one",
			old:           []gitdiff.Change{ch("M", "a.go", "p1")},
			new:           []gitdiff.Change{ch("D", "a.go", "p-deleted")},
			wantUnchanged: []string{},
			wantChanged:   []string{"a.go"},
			wantVanished:  []string{},
		},
		{
			// LoadStaged 用 --no-renames，所以重命名永远拆成 D + A 两条，
			// 不会出现 status R。两条都是新路径或新 patch，都要重审。
			name:          "a rename decomposes into a deletion and an addition",
			old:           []gitdiff.Change{ch("M", "old.go", "p1")},
			new:           []gitdiff.Change{ch("D", "old.go", "p-del"), ch("A", "new.go", "p-add")},
			wantUnchanged: []string{},
			wantChanged:   []string{"old.go", "new.go"},
			wantVanished:  []string{},
		},
		{
			name:          "everything unstaged leaves only vanished files",
			old:           []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "b.go", "p2")},
			new:           nil,
			wantUnchanged: []string{},
			wantChanged:   []string{},
			wantVanished:  []string{"a.go", "b.go"},
		},
		{
			// status 变了但 patch 一字不差，仍算未变化：模型这次会看到与上次
			// 完全相同的输入，上次的结论就依然成立。实践中 git 生成的 patch
			// 头部带 new file / deleted file，status 变了 patch 几乎必然跟着变，
			// 这个用例是在固定"判据只看 patch"这条约定本身。
			name:          "status alone does not trigger a re-review",
			old:           []gitdiff.Change{ch("A", "a.go", "p1")},
			new:           []gitdiff.Change{ch("M", "a.go", "p1")},
			wantUnchanged: []string{"a.go"},
			wantChanged:   []string{},
			wantVanished:  []string{},
		},
		{
			name:          "both snapshots empty",
			old:           nil,
			new:           nil,
			wantUnchanged: []string{},
			wantChanged:   []string{},
			wantVanished:  []string{},
		},
		{
			// 一次覆盖全部四种归类，确认它们互不干扰。
			name: "mixed snapshot splits into all three groups",
			old: []gitdiff.Change{
				ch("M", "same.go", "p1"),
				ch("M", "edited.go", "p2"),
				ch("M", "gone.go", "p3"),
			},
			new: []gitdiff.Change{
				ch("M", "same.go", "p1"),
				ch("M", "edited.go", "p2-fixed"),
				ch("A", "added.go", "p4"),
			},
			wantUnchanged: []string{"same.go"},
			wantChanged:   []string{"edited.go", "added.go"},
			wantVanished:  []string{"gone.go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiffSnapshot(tt.old, tt.new)

			if diff := paths(got.Unchanged); !reflect.DeepEqual(diff, tt.wantUnchanged) {
				t.Errorf("Unchanged = %v, want %v", diff, tt.wantUnchanged)
			}
			if diff := paths(got.Changed); !reflect.DeepEqual(diff, tt.wantChanged) {
				t.Errorf("Changed = %v, want %v", diff, tt.wantChanged)
			}
			vanished := got.Vanished
			if vanished == nil {
				vanished = []string{}
			}
			if !reflect.DeepEqual(vanished, tt.wantVanished) {
				t.Errorf("Vanished = %v, want %v", vanished, tt.wantVanished)
			}

			// Unchanged + Changed 必须正好是 new 的一个划分：不重不漏。
			// 少一个文件意味着它被静默跳过、永远不会被审查，而这种漏审
			// 在结果里看不出来——只会表现为"模型好像没注意到这个文件"。
			if n := len(got.Unchanged) + len(got.Changed); n != len(tt.new) {
				t.Errorf("Unchanged+Changed covers %d files, want %d (they must partition new)", n, len(tt.new))
			}
		})
	}
}

// TestDiffSnapshotPreservesOrderAndContent 固定住两件容易被 map 破坏的事：
// 分组内的顺序跟随 new，以及返回的是 new 里的完整 Change（含 Status 和
// Patch），而不是只有路径。
func TestDiffSnapshotPreservesOrderAndContent(t *testing.T) {
	tests := []struct {
		name string
		old  []gitdiff.Change
		new  []gitdiff.Change
	}{
		{
			name: "unchanged group follows new order",
			// 刻意让 old 的顺序与 new 相反：如果实现是遍历 old 或遍历 map，
			// 结果顺序就会跟着乱。
			old: []gitdiff.Change{ch("M", "z.go", "p3"), ch("M", "m.go", "p2"), ch("M", "a.go", "p1")},
			new: []gitdiff.Change{ch("M", "a.go", "p1"), ch("M", "m.go", "p2"), ch("M", "z.go", "p3")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DiffSnapshot(tt.old, tt.new)
			if !reflect.DeepEqual(got.Unchanged, tt.new) {
				t.Errorf("Unchanged = %+v, want it to equal new verbatim: %+v", got.Unchanged, tt.new)
			}
		})
	}
}

func TestCarryOverFindings(t *testing.T) {
	tests := []struct {
		name      string
		findings  []Finding
		unchanged []gitdiff.Change
		wantIDs   []string
	}{
		{
			name: "findings on unchanged files carry over",
			findings: []Finding{
				{ID: "f1", File: "same.go"},
				{ID: "f2", File: "edited.go"},
			},
			unchanged: []gitdiff.Change{ch("M", "same.go", "p1")},
			wantIDs:   []string{"f1"},
		},
		{
			// 用户已经论证过这条不成立，而代码根本没变。再让它出现一次
			// 就是无视用户的输入——交互功能最不该犯的错。
			name: "withdrawn findings are dropped even on unchanged files",
			findings: []Finding{
				{ID: "f1", File: "same.go"},
				{ID: "f2", File: "same.go", Status: StatusWithdrawn},
			},
			unchanged: []gitdiff.Change{ch("M", "same.go", "p1")},
			wantIDs:   []string{"f1"},
		},
		{
			// 复核丢弃的意见照样带过来：新记录应当是完整的历史，
			// 展示层自会用 ActiveFindings 把它们挡在外面。
			name: "critique-dropped findings still carry over as history",
			findings: []Finding{
				{ID: "f1", File: "same.go", Kept: true},
				{ID: "f2", File: "same.go", Kept: false, CritiqueReason: "speculative"},
			},
			unchanged: []gitdiff.Change{ch("M", "same.go", "p1")},
			wantIDs:   []string{"f1", "f2"},
		},
		{
			// 文件要重审，旧意见一概不带——重审会重新产出它自己的意见，
			// 带过来就成了重复。
			name: "nothing carries over when no file is unchanged",
			findings: []Finding{
				{ID: "f1", File: "a.go"},
				{ID: "f2", File: "b.go"},
			},
			unchanged: nil,
			wantIDs:   []string{},
		},
		{
			// 文件消失（已提交 / unstage）时它不在 unchanged 里，
			// 所以它的意见自然被挡住——针对不在暂存区的文件报意见，
			// 用户无从下手。
			name: "findings on vanished files do not carry over",
			findings: []Finding{
				{ID: "f1", File: "gone.go"},
				{ID: "f2", File: "same.go"},
			},
			unchanged: []gitdiff.Change{ch("M", "same.go", "p1")},
			wantIDs:   []string{"f2"},
		},
		{
			name:      "no findings yields none",
			findings:  nil,
			unchanged: []gitdiff.Change{ch("M", "same.go", "p1")},
			wantIDs:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CarryOverFindings(tt.findings, tt.unchanged)
			gotIDs := make([]string, 0, len(got))
			for _, f := range got {
				gotIDs = append(gotIDs, f.ID)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Errorf("CarryOverFindings IDs = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// TestCarryOverFindingsPreservesDiscussion 固定住"带过来的是完整的 Finding"：
// 用户与模型围绕这条意见的往复讨论，是判断它可不可信的依据，不能在重审时丢掉。
func TestCarryOverFindingsPreservesDiscussion(t *testing.T) {
	tests := []struct {
		name string
		in   Finding
	}{
		{
			name: "discussion and critique reason survive",
			in: Finding{
				ID: "f1", File: "same.go", Kept: true, CritiqueReason: "holds up",
				Discussion: []schema.Message{{Role: schema.RoleUser, Content: "为什么？"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CarryOverFindings([]Finding{tt.in}, []gitdiff.Change{ch("M", "same.go", "p1")})
			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			if !reflect.DeepEqual(got[0], tt.in) {
				t.Errorf("carried finding = %+v, want %+v", got[0], tt.in)
			}
		})
	}
}
