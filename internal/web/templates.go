package web

import (
	"html/template"
	"reflect"
	"strconv"
	"strings"
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
</style>
</head>
<body>
<header>
  <h1>Reviewer-AI · 审查记录</h1>
  <div class="sub">本机历史审查运行，按更新时间倒序。点击查看完整消息历史与复核前后的意见差异。</div>
</header>
<main>
{{if .}}
  <table>
    <thead><tr>
      <th style="width:150px">时间</th>
      <th style="width:110px">状态</th>
      <th>仓库</th>
      <th style="width:140px">分支</th>
      <th style="width:70px">文件</th>
      <th style="width:70px">保留</th>
      <th style="width:70px">丢弃</th>
      <th style="width:90px">操作</th>
    </tr></thead>
    <tbody>
    {{range .}}
      <tr>
        <td class="time mono">{{.UpdatedAt.Format "2006-01-02 15:04:05"}}</td>
        <td><span class="badge {{statusClass .Status}}">{{.Status}}</span></td>
        <td class="repo mono">{{.RepoPath}}</td>
        <td class="mono">{{if .Branch}}{{.Branch}}{{else}}<span class="placeholder">—</span>{{end}}</td>
        <td class="num">{{.Files}}</td>
        <td class="kept">{{.Kept}}</td>
        <td class="dropped">{{if .Critiqued}}{{.Dropped}}{{else}}<span class="placeholder">未复核</span>{{end}}</td>
        <td><a href="/run?id={{.ID}}">查看详情 →</a></td>
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
  {{if .}}
    {{range .}}
      <div class="finding">
        <div class="fhead">
          <span class="badge {{severityClass .Severity}}">{{upper .Severity}}</span>
          <span class="floc mono">{{.File}}{{lineRange .StartLine .EndLine}}</span>
          <span class="fid">{{.ID}}</span>
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
      <dt>状态</dt><dd><span class="badge {{statusClass .Run.Status}}">{{.Run.Status}}</span></dd>
      <dt>创建时间</dt><dd class="mono">{{.Run.CreatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>更新时间</dt><dd class="mono">{{.Run.UpdatedAt.Format "2006-01-02 15:04:05"}}</dd>
      <dt>复核</dt><dd>{{if .Run.Critiqued}}已完成{{else}}未执行{{end}}</dd>
      <dt>改动文件</dt><dd>{{len .Run.Snapshot}} 个</dd>
    </dl>
  </div>

  {{if .Run.Summary}}
    <h2>整体评价</h2>
    <div class="card"><pre>{{.Run.Summary}}</pre></div>
  {{end}}

  {{/* 分级折叠：审查结论（保留的意见）是打开这个页面的主要目的，默认展开；
       丢弃组和消息历史是排查时才翻的，默认折叠。用原生 <details>，零 JS。 */}}
  <details class="section" open>
    <summary><h2>保留的意见 <span class="count">（{{len .Kept}} 条{{if not .Run.Critiqued}} · 复核未执行，以下为初审原始结果{{end}}）</span></h2></summary>
    <div class="card">{{template "findings" .Kept}}</div>
  </details>

  <details class="section">
    <summary><h2>被复核丢弃的意见 <span class="count">（{{len .Dropped}} 条）</span></h2></summary>
    <div class="card dropped">{{template "findings" .Dropped}}</div>
  </details>

  <details class="section">
    <summary><h2>消息历史 <span class="count">（{{len .Run.Messages}} 条 · 工具结果内容已在落盘时压缩为占位符）</span></h2></summary>
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
