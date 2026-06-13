package catalog

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"

	"github.com/jakenesler/idp/internal/render"
)

// DiscoverEnvs lists the environments that have rendered state under clusters/ —
// every clusters/<env>/platform.yaml. This is what `catalog --all` views: the
// whole platform, every environment, as it stands in git.
func DiscoverEnvs(root string) ([]string, error) {
	dir := filepath.Join(root, "clusters")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var envs []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(render.PlatformPath(root, e.Name())); err == nil {
			envs = append(envs, e.Name())
		}
	}
	sort.Strings(envs)
	if len(envs) == 0 {
		return nil, fmt.Errorf("no rendered environments found under %s", dir)
	}
	return envs, nil
}

// PageFile is the HTML filename for an env's catalog page within a site.
func PageFile(env string) string { return env + ".html" }

// BuildSite renders a self-contained multi-env site into outDir: one
// <env>.html per environment plus an index.html linking them. It is the
// "across the board" view — the whole platform at a glance, publishable as-is
// (e.g. to GitHub Pages). Returns the env pages written, in order.
func BuildSite(root, outDir string) ([]string, error) {
	envs, err := DiscoverEnvs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", outDir, err)
	}
	cats := make([]*Catalog, 0, len(envs))
	for _, env := range envs {
		c, err := Load(root, env)
		if err != nil {
			return nil, err
		}
		// The directory under clusters/ is the canonical env identity, so page
		// filenames and index links agree even if a platform.yaml's inner env
		// field ever drifts from its directory.
		c.Env = env
		html, err := RenderHTML(c)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, PageFile(env)), html, 0o644); err != nil {
			return nil, err
		}
		cats = append(cats, c)
	}
	index, err := RenderIndexHTML(cats)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "index.html"), index, 0o644); err != nil {
		return nil, err
	}
	return envs, nil
}

// RenderIndexHTML builds the site landing page: one card per environment with
// its counts, linking to that env's catalog page. Deterministic (no timestamp).
func RenderIndexHTML(cats []*Catalog) ([]byte, error) {
	type envCard struct {
		Env       string
		Page      string
		Products  int
		Workloads int
		Modules   int
		Source    string
	}
	cards := make([]envCard, 0, len(cats))
	for _, c := range cats {
		cards = append(cards, envCard{
			Env: c.Env, Page: PageFile(c.Env), Products: c.Products(),
			Workloads: len(c.Apps), Modules: len(c.Modules), Source: c.Source,
		})
	}
	t, err := template.New("index").Parse(indexTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, cards); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const indexTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>idp catalog</title>
<style>
  :root { --bg:#f5f6f8; --card:#fff; --ink:#1c2024; --muted:#6b7280; --line:#e5e7eb; --accent:#4f46e5;
          --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace; }
  * { box-sizing: border-box; }
  body { margin:0; background:var(--bg); color:var(--ink);
         font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif; }
  header.top { background:#14161a; color:#f3f4f6; padding:22px 28px; }
  header.top h1 { font-size:18px; margin:0; font-weight:600; }
  header.top h1 .dim { color:#9ca3af; font-weight:400; }
  .sub { padding:10px 28px; color:var(--muted); font-size:13px; background:#eceef1; border-bottom:1px solid var(--line); }
  main { padding:28px; max-width:900px; margin:0 auto; }
  .grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(240px,1fr)); gap:16px; }
  a.card { display:block; text-decoration:none; color:inherit; background:var(--card);
           border:1px solid var(--line); border-radius:12px; padding:18px 20px;
           box-shadow:0 1px 2px rgba(16,24,40,.04); transition:box-shadow .12s, transform .12s; }
  a.card:hover { box-shadow:0 6px 18px rgba(16,24,40,.10); transform:translateY(-1px); }
  a.card .env { font:600 18px/1.2 var(--mono); color:var(--accent); }
  a.card .nums { margin-top:10px; color:var(--muted); font-size:13px; }
  a.card .nums b { color:var(--ink); }
  a.card .src { margin-top:8px; color:var(--muted); font-size:12px; font-family:var(--mono); }
  footer { color:var(--muted); font-size:12px; padding:0 28px 36px; max-width:900px; margin:0 auto; }
  footer code { font-family:var(--mono); }
</style>
</head>
<body>
<header class="top"><h1>idp <span class="dim">·</span> platform catalog</h1></header>
<div class="sub">A read-only projection of every environment's committed desired state. Pick an environment.</div>
<main>
  <div class="grid">
    {{range .}}
    <a class="card" href="{{.Page}}">
      <div class="env">{{.Env}}</div>
      <div class="nums"><b>{{.Products}}</b> product(s) · <b>{{.Workloads}}</b> workload(s) · <b>{{.Modules}}</b> module(s)</div>
      {{if .Source}}<div class="src">source: {{.Source}}</div>{{end}}
    </a>
    {{end}}
  </div>
</main>
<footer>Generated by <code>idpctl catalog --all</code> — a read-only view. Source of truth: <code>clusters/&lt;env&gt;/platform.yaml</code>.</footer>
</body>
</html>
`
