package web

import (
	"html/template"
	"reflect"
	"strconv"
	"strings"

	"github.com/qizhaoTan/Reviewer-AI/internal/review"
)

// tmplFuncs 是两个模板共用的辅助函数。放在模板里做的都是纯展示逻辑
// （格式化行号、把 status 映射成 CSS class），不含任何业务判断。
var tmplFuncs = template.FuncMap{
	// lineRange 把 StartLine/EndLine 渲染成 ":12" / ":12-14"，未定位到时
	// 返回空串（该意见按文件级展示）。
	"lineRange": func(start, end int) string {
		if start <= 0 {
			return ""
		}
		if end > start {
			return ":" + strconv.Itoa(start) + "-" + strconv.Itoa(end)
		}
		return ":" + strconv.Itoa(start)
	},
	// statusClass 把运行状态映射成 CSS class 名，供徽标着色。
	"statusClass": func(s any) string {
		return "st-" + strings.ReplaceAll(toString(s), "_", "-")
	},
	// severityClass 同上，用于意见的严重级别徽标。
	"severityClass": func(s any) string {
		return "sev-" + toString(s)
	},
	"upper": func(s any) string { return strings.ToUpper(toString(s)) },
	// peek 取正文的一小段作为折叠状态下的预览，让人不必挨个展开就能认出
	// 哪条消息是自己要找的。换行折成空格，保证摘要行不被撑成多行。
	"peek": func(s string) string { return truncateRunes(strings.TrimSpace(s), peekMaxRunes) },
	// findingGroup 把子模板需要的三样东西打包成一个值，见 findingGroupData。
	"findingGroup": func(runID string, interactive bool, findings []review.Finding) findingGroupData {
		return findingGroupData{RunID: runID, Interactive: interactive, Findings: findings}
	},
}

// findingGroupData 是 "findings" 子模板的入参。
//
// 用一个结构体而不是直接传 []review.Finding：子模板要渲染回复表单，就需要
// 知道运行 ID（表单要带上），也需要知道这一组该不该出现表单（被复核砍掉的
// 那组不该）。Go 模板里没法从 range 内部回头拿外层数据，只能一起传进来。
type findingGroupData struct {
	RunID       string
	Interactive bool
	Findings    []review.Finding
}

// peekMaxRunes 是折叠消息预览的最大长度（rune 数）。
const peekMaxRunes = 80

// truncateRunes 按 rune 截断，避免把一个多字节汉字切成半个乱码
// （按 byte 截断就会）。同时把换行折成空格，让预览始终是单行。
func truncateRunes(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max]) + "…"
}

// toString 把模板传进来的具名字符串类型（store.RunStatus、review.Severity）
// 转成 string。
//
// 用 reflect 而不是 `case string` 类型断言：具名字符串类型的动态类型是
// store.RunStatus 而非 string，断言匹配不上会静默返回空串——徽标于是丢掉
// 颜色 class、文字也变空，页面看着"只是没样式"，不会报错。reflect.Kind
// 认的是底层类型，这类具名类型都能正确取到值。
func toString(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.String {
		return ""
	}
	return rv.String()
}

// sharedCSS 被两个页面共用，避免同一套配色改两处。
const sharedCSS = `
  body { font-family: -apple-system, "Segoe UI", "PingFang SC", sans-serif; margin: 0; background: #f6f7f9; color: #1f2329; }
  header { background: #1f2329; color: #fff; padding: 16px 24px; }
  header h1 { margin: 0; font-size: 18px; font-weight: 600; }
  header h1 a { color: #fff; text-decoration: none; }
  header .sub { color: #9aa0a6; font-size: 13px; margin-top: 4px; }
  main { padding: 24px; max-width: 1440px; margin: 0 auto; }
  a { color: #3370ff; text-decoration: none; }
  a:hover { text-decoration: underline; }
  .empty { padding: 48px; text-align: center; color: #8a9099; background: #fff; border-radius: 8px; }
  .mono { font-family: ui-monospace, Menlo, Consolas, monospace; }
  pre { font-family: ui-monospace, Menlo, Consolas, monospace; font-size: 13px; line-height: 1.55;
        white-space: pre-wrap; overflow-wrap: anywhere; margin: 0; }
  .badge { display: inline-block; padding: 2px 8px; border-radius: 10px; font-size: 12px; font-weight: 600; }
  .st-completed   { background: #e6f4ea; color: #137333; }
  .st-in-progress { background: #fef7e0; color: #b06000; }
  .st-failed      { background: #fce8e6; color: #c5221f; }
  .st-pending     { background: #eef0f2; color: #646a73; }
  .sev-error   { background: #fce8e6; color: #c5221f; }
  .sev-warning { background: #fef7e0; color: #b06000; }
  .sev-info    { background: #e8f0fe; color: #1a56c4; }
  .banner { padding: 12px 16px; border-radius: 8px; margin-bottom: 16px; font-size: 14px; }
  .banner.ok  { background: #e6f4ea; color: #137333; }
  .banner.err { background: #fce8e6; color: #c5221f; }
`

const indexHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Reviewer-AI · 审查记录</title>
<style>` + sharedCSS + `
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,.08); }
  th, td { text-align: left; padding: 12px 16px; border-bottom: 1px solid #eef0f2; font-size: 14px; }
  th { background: #fafbfc; color: #646a73; font-weight: 600; }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: #f8f9fb; }
  td.repo { overflow-wrap: anywhere; }
  td.time, td.num { color: #646a73; font-size: 13px; }
  td.kept { font-weight: 600; }
  td.dropped { color: #8a9099; }
  .placeholder { color: #b0b5bb; }
  td.ops { white-space: nowrap; }
  .del { display: inline; margin-left: 12px; }
  .del button { background: none; border: none; padding: 0; font: inherit; font-size: 14px;
                color: #c5221f; cursor: pointer; }
  .del button:hover { text-decoration: underline; }

  /* 清空全部单独一行、靠右，与表格内的行级删除按钮拉开距离——
     两个破坏力差一个数量级的操作贴在一起，迟早会点错。 */
  .toolbar { display: flex; justify-content: flex-end; margin-bottom: 12px; }
  .toolbar button { background: #fff; color: #c5221f; border: 1px solid #f5c6c2;
                    border-radius: 6px; padding: 6px 14px; font-size: 13px; cursor: pointer; }
  .toolbar button:hover { background: #fce8e6; }
</style>
</head>
<body>
<header>
  <h1>Reviewer-AI · 审查记录</h1>
  <div class="sub">本机历史审查运行，按更新时间倒序。点击查看完整消息历史与复核前后的意见差异。</div>
</header>
<main>
{{if .Notice}}<div class="banner ok">{{.Notice}}</div>{{end}}
{{if .Error}}<div class="banner err">{{.Error}}</div>{{end}}
{{if .Rows}}
  {{/* 确认文案带上条数，让用户点之前就知道这一下要毁掉多少东西。
       说"至少 N 条"是因为列表页有展示上限，库里可能比页面上看到的更多。 */}}
  <form class="toolbar" method="post" action="/delete-all"
        onsubmit="return confirm('清空全部审查记录？至少 {{.Total}} 条记录及其消息历史会被一并删除，无法恢复。')">
    <button type="submit">清空全部记录</button>
  </form>
  <table>
    <thead><tr>
      <th style="width:150px">时间</th>
      <th style="width:110px">状态</th>
      <th>仓库</th>
      <th style="width:140px">分支</th>
      <th style="width:70px">文件</th>
      <th style="width:70px">保留</th>
      <th style="width:70px">丢弃</th>
      <th style="width:160px">操作</th>
    </tr></thead>
    <tbody>
    {{range .Rows}}
      <tr>
        <td class="time mono">{{.UpdatedAt.Format "2006-01-02 15:04:05"}}</td>
        <td><span class="badge {{statusClass .Status}}">{{.Status}}</span></td>
        <td class="repo mono">{{.RepoPath}}</td>
        <td class="mono">{{if .Branch}}{{.Branch}}{{else}}<span class="placeholder">—</span>{{end}}{{if .BaseRev}} <span class="placeholder">→ {{.BaseRev}}</span>{{end}}</td>
        <td class="num">{{.Files}}</td>
        <td class="kept">{{.Kept}}</td>
        <td class="dropped">{{if .Critiqued}}{{.Dropped}}{{else}}<span class="placeholder">未复核</span>{{end}}</td>
        <td class="ops">
          <a href="/run?id={{.ID}}">查看详情 →</a>
          {{/* 删除不可撤销（整条消息历史一起没了），所以拦一道 confirm。
               用内联 onsubmit 而不是引一个 JS 文件：一行就够，不值得为它
               破坏"零前端依赖"这个约定。 */}}
          <form class="del" method="post" action="/delete"
                onsubmit="return confirm('删除这条审查记录？消息历史与全部意见都会一并删除，无法恢复。')">
            <input type="hidden" name="run" value="{{.ID}}">
            <button type="submit" title="删除后可对同一改动重新审查">删除</button>
          </form>
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
{{else}}
  <div class="empty">还没有任何审查记录。先在仓库里 stage 一些改动跑一次 reviewer-ai。</div>
{{end}}
</main>
</body>
</html>
`

// runHTML 详情页。findings 块被两组意见（保留 / 丢弃）复用，所以抽成
// 一个 define；两组的差异只有标题和是否显示复核理由。
const runHTML = `{{define "findings"}}
  {{$runID := .RunID}}{{$interactive := .Interactive}}
  {{if .Findings}}
    {{range .Findings}}
      <div class="finding{{if .Status.IsWithdrawn}} withdrawn{{end}}">
        <div class="fhead">
          <span class="badge {{severityClass .Severity}}">{{upper .Severity}}</span>
          <span class="floc mono">{{.File}}{{lineRange .StartLine .EndLine}}</span>
          <span class="fid">{{.ID}}</span>
          {{if .Status.IsWithdrawn}}<span class="badge wd">已撤回</span>{{end}}
          {{if not .StartLine}}<span class="fnote">未定位到行号，按文件级意见处理</span>{{end}}
        </div>
        <div class="fsummary">{{.Summary}}</div>
        {{/* 摘要（级别 + 位置 + 一句话）常显，其余细节折叠：一屏能扫完所有
             意见，想深究某一条再点开。三块细节都为空时不渲染这个折叠框，
             免得点开是空的。 */}}
        {{if or .Detail .Anchor .CritiqueReason}}
          <details class="fdetails">
            <summary>详情{{if .Anchor}} / anchor{{end}}{{if .CritiqueReason}} / 复核理由{{end}}</summary>
            <div class="fbody">
              {{if .Detail}}<div class="fdetail"><pre>{{.Detail}}</pre></div>{{end}}
              {{if .Anchor}}
                <div class="flabel">anchor（模型引用的代码）</div>
                <div class="fanchor"><pre>{{.Anchor}}</pre></div>
              {{end}}
              {{if .CritiqueReason}}
                <div class="flabel">复核理由</div>
                <div class="freason"><pre>{{.CritiqueReason}}</pre></div>
              {{end}}
            </div>
          </details>
        {{end}}

        {{/* 讨论记录默认展开：用户提过异议的意见，那段往复正是他下次回来
             要看的东西，藏起来等于白讨论一场。 */}}
        {{if .Discussion}}
          <details class="fdiscuss" open>
            <summary>讨论记录（{{len .Discussion}} 条）</summary>
            <div class="dbody">
              {{range .Discussion}}
                <div class="dmsg">
                  <span class="role role-{{.Role}}">{{.Role}}</span>
                  {{range .ToolCalls}}<span class="tag tool">{{.Name}}</span>{{end}}
                  {{if .Content}}<pre>{{.Content}}</pre>{{end}}
                </div>
              {{end}}
            </div>
          </details>
        {{end}}

        {{/* 回复框只给还没撤回的意见：已经撤回的再争论没有意义。
             Dropped 组传进来的 Interactive 是 false，所以那一组不渲染。 */}}
        {{if and $interactive (not .Status.IsWithdrawn)}}
          <form class="freply" method="post" action="/reply">
            <input type="hidden" name="run" value="{{$runID}}">
            <input type="hidden" name="finding" value="{{.ID}}">
            <textarea name="reply" rows="2" placeholder="不同意这条意见？说明理由，模型会去核对代码后决定是否撤回。"></textarea>
            <button type="submit">提交回复</button>
          </form>
        {{end}}
      </div>
    {{end}}
  {{else}}
    <div class="none">（无）</div>
  {{end}}
{{end}}<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Reviewer-AI · 运行详情</title>
<style>` + sharedCSS + `
  .meta { background: #fff; border-radius: 8px; padding: 16px 20px; box-shadow: 0 1px 3px rgba(0,0,0,.08); margin-bottom: 20px; }
  .meta dl { display: grid; grid-template-columns: 110px 1fr; gap: 8px 16px; margin: 0; font-size: 14px; }
  .meta dt { color: #646a73; }
  .meta dd { margin: 0; overflow-wrap: anywhere; }
  h2 { font-size: 16px; margin: 24px 0 12px; }
  h2 .count { color: #8a9099; font-weight: 400; font-size: 14px; }
  .card { background: #fff; border-radius: 8px; padding: 16px 20px; box-shadow: 0 1px 3px rgba(0,0,0,.08); }
  .finding { border-left: 3px solid #eef0f2; padding: 12px 0 12px 14px; margin-bottom: 14px; }
  .finding:last-child { margin-bottom: 0; }
  .fhead { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; margin-bottom: 6px; }
  .floc { font-size: 13px; color: #1f2329; }
  .fid { font-size: 12px; color: #b0b5bb; }
  .fnote { font-size: 12px; color: #b06000; }
  .fsummary { font-size: 14px; margin-bottom: 8px; }
  .fdetail, .fanchor, .freason { background: #fafbfc; border-radius: 6px; padding: 10px 12px; margin-bottom: 8px; }
  .fanchor { background: #f2f6ff; }
  .flabel { font-size: 12px; color: #8a9099; margin-bottom: 4px; }
  .dropped .finding { opacity: .72; }
  .finding.withdrawn { opacity: .6; }
  .finding.withdrawn .fsummary { text-decoration: line-through; }
  .badge.wd { background: #eef0f2; color: #646a73; }

  .rereview { background: #fff; border-radius: 8px; padding: 16px 20px; margin-bottom: 20px;
              box-shadow: 0 1px 3px rgba(0,0,0,.08); display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
  .rereview button { background: #3370ff; color: #fff; border: none; border-radius: 6px;
                     padding: 8px 16px; font-size: 14px; cursor: pointer; }
  .rereview button:disabled { background: #b0b5bb; cursor: progress; }
  .rereview .hint { color: #8a9099; font-size: 13px; }

  .freply { margin-top: 10px; display: flex; gap: 8px; align-items: flex-start; }
  .freply textarea { flex: 1; font-family: inherit; font-size: 13px; padding: 8px 10px;
                     border: 1px solid #dee0e3; border-radius: 6px; resize: vertical; }
  .freply button { background: #f2f6ff; color: #1a56c4; border: 1px solid #d0dcff;
                   border-radius: 6px; padding: 8px 14px; font-size: 13px; cursor: pointer; white-space: nowrap; }
  .freply button:hover { background: #e4ecff; }

  .fdiscuss { margin-top: 10px; }
  .fdiscuss > summary { cursor: pointer; font-size: 12px; color: #8a9099; padding: 2px 0; }
  .fdiscuss .dbody { margin-top: 8px; }
  .dmsg { background: #fafbfc; border-radius: 6px; padding: 8px 10px; margin-bottom: 6px; }
  .dmsg pre { margin-top: 6px; }
  .none { color: #b0b5bb; font-size: 14px; }

  /* 折叠区块：summary 里放 h2，去掉 h2 自带的外边距免得撑开三角标记。 */
  .section { margin-bottom: 8px; }
  .section > summary { cursor: pointer; list-style: none; }
  .section > summary::-webkit-details-marker { display: none; }
  .section > summary h2 { display: inline; }
  .section > summary::before { content: "▸ "; color: #8a9099; font-size: 13px; }
  .section[open] > summary::before { content: "▾ "; }
  .section > summary:hover h2 { color: #3370ff; }

  /* 单条意见内部的细节折叠。 */
  .fdetails > summary { cursor: pointer; font-size: 12px; color: #8a9099; padding: 2px 0; }
  .fdetails > summary:hover { color: #3370ff; }
  .fdetails > .fbody { margin-top: 8px; }

  /* 单条消息折叠：角色徽标与预览常显，正文点开才渲染出来。 */
  .msg { border-top: 1px solid #eef0f2; padding: 10px 0; }
  .msg:first-child { border-top: none; }
  .msg > summary { cursor: pointer; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .msg > summary:hover .peek { color: #3370ff; }
  .msg > .mcontent { margin-top: 10px; }
  .peek { color: #8a9099; font-size: 13px; overflow-wrap: anywhere; }
  .tag.tool { background: #f2f6ff; color: #1a56c4; padding: 1px 6px; border-radius: 4px; }
  .mhead { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 8px; }
  .role { font-size: 12px; font-weight: 600; padding: 2px 8px; border-radius: 10px; }
  .role-system { background: #eef0f2; color: #646a73; }
  .role-user { background: #e8f0fe; color: #1a56c4; }
  .role-assistant { background: #e6f4ea; color: #137333; }
  .tag { font-size: 12px; color: #8a9099; font-family: ui-monospace, Menlo, Consolas, monospace; }
  .mbody { background: #fafbfc; border-radius: 6px; padding: 10px 12px; }
  .toolcall { background: #f2f6ff; border-radius: 6px; padding: 10px 12px; margin-top: 8px; }
  .reasoning { background: #fff8e6; border-radius: 6px; padding: 10px 12px; margin-top: 8px; }
</style>
</head>
<body>
<header>
  <h1><a href="/">Reviewer-AI · 审查记录</a></h1>
  <div class="sub">运行详情</div>
</header>
<main>
  <div class="meta">
    <dl>
      <dt>运行 ID</dt><dd class="mono">{{.Run.ID}}</dd>
      <dt>仓库</dt><dd class="mono">{{.RepoPath}}</dd>
      <dt>分支</dt><dd class="mono">{{if .Branch}}{{.Branch}}{{else}}—{{end}}</dd>
      {{if .BaseRev}}<dt>对比基准</dt><dd class="mono">{{.BaseRev}}</dd>{{end}}
      <dt>状态</dt><dd><span class="badge {{statusClass .Run.Status}}">{{.Run.Status}}</span></dd>
      <dt>创建时间</dt><dd class="mono">{{.Run.CreatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>更新时间</dt><dd class="mono">{{.Run.UpdatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>复核</dt><dd>{{if .Run.Critiqued}}已完成{{else}}未执行{{end}}</dd>
      <dt>改动文件</dt><dd>{{len .Run.Snapshot}} 个</dd>
      {{if .Run.ParentRunID}}
        <dt>上一轮</dt><dd><a href="/run?id={{.Run.ParentRunID}}">{{.Run.ParentRunID}} →</a></dd>
      {{end}}
    </dl>
  </div>

  {{if .Notice}}<div class="banner ok">{{.Notice}}</div>{{end}}
  {{if .Error}}<div class="banner err">{{.Error}}</div>{{end}}

  {{if .Interactive}}
    {{/* 重审按钮同步阻塞，可能跑几分钟，所以按钮上写清楚这一点，
         免得用户以为页面卡死了反复点。 */}}
    <form class="rereview" method="post" action="/rereview"
          onsubmit="this.querySelector('button').disabled=true;this.querySelector('button').textContent='正在重新审查，请勿关闭页面…'">
      <input type="hidden" name="run" value="{{.Run.ID}}">
      <button type="submit">我已按意见修改并重新 stage，增量重审</button>
      <span class="hint">只有 patch 真正变过的文件会重新送审，未改动文件的意见原样保留。</span>
    </form>
  {{end}}

  {{if .Run.Summary}}
    <h2>整体评价</h2>
    <div class="card"><pre>{{.Run.Summary}}</pre></div>
  {{end}}

  {{/* 分级折叠：审查结论（保留的意见）是打开这个页面的主要目的，默认展开；
       丢弃组和消息历史是排查时才翻的，默认折叠。用原生 <details>，零 JS。 */}}
  <details class="section" open>
    <summary><h2>保留的意见 <span class="count">（{{len .Kept}} 条{{if not .Run.Critiqued}} · 复核未执行，以下为初审原始结果{{end}}）</span></h2></summary>
    <div class="card">{{template "findings" findingGroup .Run.ID .Interactive .Kept}}</div>
  </details>

  <details class="section">
    <summary><h2>被复核丢弃的意见 <span class="count">（{{len .Dropped}} 条）</span></h2></summary>
    <div class="card dropped">{{template "findings" findingGroup .Run.ID false .Dropped}}</div>
  </details>

  <details class="section">
    <summary><h2>消息历史 <span class="count">（{{len .Run.Messages}} 条{{if .ToolResultsCompacted}} · 工具结果内容已在落盘时压缩为占位符{{end}}）</span></h2></summary>
    <div class="card">
      {{range .Run.Messages}}
        {{/* 消息历史展开后仍然很长（真实运行 40+ 条起步），所以每条消息
             再折叠一层：角色和 tool_call_id 常显，正文点开才看。 */}}
        <details class="msg">
          <summary>
            <span class="role role-{{.Role}}">{{.Role}}</span>
            {{if .ToolCallID}}<span class="tag">tool_call_id: {{.ToolCallID}}</span>{{end}}
            {{range .ToolCalls}}<span class="tag tool">{{.Name}}</span>{{end}}
            <span class="peek">{{peek .Content}}</span>
          </summary>
          <div class="mcontent">
            {{if .Content}}<div class="mbody"><pre>{{.Content}}</pre></div>{{end}}
            {{if .ReasoningContent}}
              <div class="reasoning"><pre>{{.ReasoningContent}}</pre></div>
            {{end}}
            {{range .ToolCalls}}
              <div class="toolcall"><pre>{{.Name}}  [id={{.ID}}]
{{printf "%s" .Arguments}}</pre></div>
            {{end}}
          </div>
        </details>
      {{else}}
        <div class="none">（无消息历史）</div>
      {{end}}
    </div>
  </details>
</main>
</body>
</html>
`
