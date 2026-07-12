package catalog

import (
	"bytes"
	"html/template"
	"strings"
)

// RenderHTML produces a single self-contained HTML page (inline CSS, no external
// fonts/scripts/CDN) — the viewer. It is a pure function of the catalog, with no
// timestamp, so re-rendering unchanged state yields an identical file (clean
// diffs when published to e.g. GitHub Pages from CI).
func RenderHTML(c *Catalog) ([]byte, error) {
	t, err := template.New("catalog").Funcs(template.FuncMap{
		"join": strings.Join,
	}).Parse(htmlTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Products counts distinct apps (a multi-component product counts once).
func (c *Catalog) Products() int {
	seen := map[string]struct{}{}
	for _, a := range c.Apps {
		seen[a.Name] = struct{}{}
	}
	return len(seen)
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>idp catalog — {{.Env}}</title>
<style>
  :root {
    --bg: #f5f6f8; --card: #ffffff; --ink: #1c2024; --muted: #6b7280;
    --line: #e5e7eb; --accent: #4f46e5; --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--ink);
    font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
  }
  header.top {
    background: #14161a; color: #f3f4f6; padding: 22px 28px;
    display: flex; align-items: baseline; gap: 16px; flex-wrap: wrap;
  }
  header.top h1 { font-size: 18px; margin: 0; font-weight: 600; letter-spacing: .2px; }
  header.top h1 .dim { color: #9ca3af; font-weight: 400; }
  .env-pill {
    background: var(--accent); color: #fff; padding: 2px 12px; border-radius: 999px;
    font: 600 13px/1.6 var(--mono); text-transform: lowercase;
  }
  header.top .counts { margin-left: auto; color: #9ca3af; font-size: 13px; }
  header.top .counts b { color: #e5e7eb; }
  .sub { padding: 10px 28px; color: var(--muted); font-size: 13px; background: #eceef1; border-bottom: 1px solid var(--line); }
  .sub code { font-family: var(--mono); color: #374151; }
  main { padding: 24px 28px 48px; max-width: 1200px; margin: 0 auto; }
  h2.section { font-size: 13px; text-transform: uppercase; letter-spacing: .08em; color: var(--muted); margin: 28px 0 12px; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(330px, 1fr)); gap: 16px; }
  .card { background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 16px 18px; box-shadow: 0 1px 2px rgba(16,24,40,.04); }
  .card .name { font: 600 15px/1.3 var(--mono); word-break: break-all; }
  .card .product { color: var(--muted); font-size: 12px; margin-top: 2px; }
  .badges { display: flex; flex-wrap: wrap; gap: 6px; margin: 10px 0 4px; }
  .badge { font-size: 11px; font-weight: 600; padding: 2px 9px; border-radius: 999px; letter-spacing: .02em; }
  .b-public { background: #fef3c7; color: #92400e; }
  .b-internal { background: #eef2f7; color: #475569; }
  .b-lan { background: #cffafe; color: #155e75; }
  .b-worker { background: #ede9fe; color: #5b21b6; }
  .b-auto { background: #dcfce7; color: #166534; }
  .b-comp { background: #e0e7ff; color: #3730a3; }
  dl { display: grid; grid-template-columns: auto 1fr; gap: 4px 12px; margin: 12px 0 0; font-size: 13px; }
  dl dt { color: var(--muted); }
  dl dd { margin: 0; font-family: var(--mono); font-size: 12.5px; word-break: break-all; }
  .rows { margin-top: 12px; border-top: 1px solid var(--line); padding-top: 10px; font-size: 13px; }
  .rows .lbl { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: .06em; margin-bottom: 4px; }
  .rows .item { font-family: var(--mono); font-size: 12.5px; padding: 1px 0; word-break: break-all; }
  .rows .item .t { color: var(--muted); }
  table.mods { width: 100%; border-collapse: collapse; background: var(--card); border: 1px solid var(--line); border-radius: 12px; overflow: hidden; font-size: 13px; }
  table.mods th, table.mods td { text-align: left; padding: 9px 14px; border-bottom: 1px solid var(--line); }
  table.mods th { background: #f9fafb; color: var(--muted); font-weight: 600; font-size: 12px; text-transform: uppercase; letter-spacing: .05em; }
  table.mods tr:last-child td { border-bottom: 0; }
  table.mods td.mono, table.mods td .mono { font-family: var(--mono); font-size: 12.5px; }
  .empty { color: var(--muted); font-style: italic; }
  footer { color: var(--muted); font-size: 12px; padding: 0 28px 36px; max-width: 1200px; margin: 0 auto; }
  footer code { font-family: var(--mono); }
</style>
</head>
<body>
<header class="top">
  <h1>idp <span class="dim">·</span> platform catalog</h1>
  <span class="env-pill">{{.Env}}</span>
  <span class="counts"><b>{{.Products}}</b> product(s) · <b>{{len .Apps}}</b> workload(s) · <b>{{len .Modules}}</b> module(s){{if .Source}} · source <b>{{.Source}}</b>{{end}}</span>
</header>
<div class="sub">A read-only projection of <code>clusters/{{.Env}}/platform.yaml</code> — the committed desired state Flux reconciles. Nothing here writes to a cluster.</div>
<main>
  <h2 class="section">Workloads</h2>
  {{if .Apps}}
  <div class="grid">
    {{range .Apps}}
    <div class="card">
      <div class="name">{{.Workload}}</div>
      <div class="product">{{.Name}}{{if .Component}} · component <strong>{{.Component}}</strong>{{end}}</div>
      <div class="badges">
        {{if .Component}}<span class="badge b-comp">{{.Component}}</span>{{end}}
        {{if .Worker}}<span class="badge b-worker">worker</span>{{end}}
        {{range .Routes}}{{if .Public}}<span class="badge b-public">public</span>{{else}}<span class="badge b-internal">internal</span>{{end}}{{end}}
        {{if .LAN}}<span class="badge b-lan">lan</span>{{end}}
        {{if .Autoscale}}<span class="badge b-auto">autoscale {{.Autoscale.Min}}–{{.Autoscale.Max}}</span>{{end}}
      </div>
      <dl>
        <dt>image</dt><dd>{{if .Image}}{{.Image}}{{else}}—{{end}}</dd>
        <dt>namespace</dt><dd>{{.Namespace}}</dd>
        <dt>runtime</dt><dd>{{if .Worker}}no Service{{else}}port {{.Port}}{{end}} · {{.Replicas}} replica(s)</dd>
      </dl>
      {{if .Routes}}
      <div class="rows"><div class="lbl">Routes</div>
        {{range .Routes}}<div class="item">{{.Host}} <span class="t">({{if .Public}}public{{else}}internal{{end}}{{if .Humans}}, access{{end}}{{if .ServiceToken}}, token{{end}})</span></div>{{end}}
      </div>{{end}}
      {{if .LAN}}
      <div class="rows"><div class="lbl">LAN (MetalLB)</div>
        <div class="item">{{if .LAN.IP}}{{.LAN.IP}}{{else}}<span class="t">pool-assigned</span>{{end}}{{if .LAN.Port}} <span class="t">:{{.LAN.Port}}</span>{{end}}</div>
      </div>{{end}}
      {{if or .DBs .Caches}}
      <div class="rows"><div class="lbl">Data</div>
        {{range .DBs}}<div class="item">{{.Name}} <span class="t">({{.Type}}{{if .URLKeys}} → {{join .URLKeys ", "}}{{end}})</span></div>{{end}}
        {{range .Caches}}<div class="item">{{.Name}} <span class="t">({{.Type}}{{if .URLKeys}} → {{join .URLKeys ", "}}{{end}})</span></div>{{end}}
      </div>{{end}}
      {{if .Stores}}
      <div class="rows"><div class="lbl">Provisions</div>
        {{range .Stores}}<div class="item">{{.Tool}} <span class="t">→ {{.Namespace}}</span></div>{{end}}
      </div>{{end}}
      {{if .Secret}}
      <div class="rows"><div class="lbl">Secrets</div>
        <div class="item">backend {{.Secret.Backend}}{{if .Secret.Key}} <span class="t">· {{.Secret.Key}}</span>{{end}}</div>
      </div>{{end}}
    </div>
    {{end}}
  </div>
  {{else}}<p class="empty">No workloads rendered for this environment yet.</p>{{end}}

  <h2 class="section">Modules</h2>
  {{if .Modules}}
  <table class="mods">
    <thead><tr><th>Module</th><th>Source</th><th>Chart</th><th>Version</th><th>Namespace</th></tr></thead>
    <tbody>
      {{range .Modules}}
      <tr>
        <td><strong>{{.Name}}</strong></td>
        <td>{{.Source}}</td>
        <td class="mono">{{.Chart}}</td>
        <td class="mono">{{if .Version}}{{.Version}}{{else}}—{{end}}</td>
        <td class="mono">{{.Namespace}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{else}}<p class="empty">No modules enabled for this environment.</p>{{end}}
</main>
<footer>Generated by <code>idpctl catalog</code> — a read-only view. The source of truth is git: <code>clusters/{{.Env}}/platform.yaml</code>, reconciled by Flux.</footer>
</body>
</html>
`
