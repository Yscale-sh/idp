// idp-shipper is the infra-owned, in-cluster orchestrator that realizes the
// platform's "push to master -> it deploys" contract. Developers only ever touch
// their deploy.yaml shopping list; the shipper holds ALL the orchestration:
//
//	for each registered app, every interval:
//	  sha := github head(app.repo, app.branch)
//	  if sha changed:
//	    derive the build set from the shopping lists (dedup by image)
//	    build each image via the homelab image-builder  (internal/builder)
//	    render each component into the platform umbrella (internal/deploy)
//	    git commit + push the platform repo             -> Flux reconciles
//
// Build detail (context/dockerfile/submodules) is read from each deploy.yaml's
// `build:` block — the developer self-serves bundling with no registry change.
// The shipper reuses idpctl's render core (single writer of the umbrella) and the
// kube kubectl wrapper (no client-go).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/builder"
	"github.com/jakenesler/idp/internal/clusterenv"
	"github.com/jakenesler/idp/internal/deploy"
	"github.com/jakenesler/idp/internal/kube"
)

var (
	httpClient = &http.Client{Timeout: 30 * time.Second} // never let a hung GitHub call freeze shipping
	redactor   *strings.Replacer                         // scrubs the token from any logged string (defense in depth)
)

// Registry is the infra-owned config (a ConfigMap) of apps to ship. It says only
// WHERE each app's shopping lists live — never how to build them (that's in the
// deploy.yaml). The developer's repo holds none of this.
type Registry struct {
	Env             string    `json:"env"`             // target environment (dev|prod)
	PlatformRepo    string    `json:"platformRepo"`    // org/name of the idp repo to render+push
	PlatformBranch  string    `json:"platformBranch"`  // branch Flux reconciles (default main)
	PlatformRoot    string    `json:"platformRoot"`    // checkout dir in the pod (holds clusters/, environments/)
	IntervalSeconds int       `json:"intervalSeconds"` // poll cadence
	Apps            []AppSpec `json:"apps"`
}

// AppSpec registers one application: which repo/branch to watch and which
// deploy.yaml shopping lists it has. Nothing build-specific lives here.
type AppSpec struct {
	Name        string   `json:"name"`        // logical app name (for logs)
	Repo        string   `json:"repo"`        // org/name to watch + build
	Branch      string   `json:"branch"`      // branch to track (e.g. master)
	DeployFiles []string `json:"deployFiles"` // deploy.yaml paths within the repo
}

// component is a parsed shopping list.
type component struct {
	file string
	cfg  appconfig.App
}

// buildTarget is one image to build, derived from the shopping lists.
type buildTarget struct {
	Image      string
	Context    string
	Dockerfile string
	Submodules []string
}

func main() {
	regPath := envOr("REGISTRY_PATH", "/etc/idp-shipper/registry.yaml")
	token := os.Getenv("GIT_TOKEN") // GitHub token: read app repos + push platform repo
	if token == "" {
		fatalf("GIT_TOKEN is required (GitHub token with repo scope)")
	}
	redactor = strings.NewReplacer(token, "***")
	reg, err := loadRegistry(regPath)
	if err != nil {
		fatalf("load registry %s: %v", regPath, err)
	}
	ctx := context.Background()
	if err := setupGitAuth(); err != nil {
		fatalf("git auth setup: %v", err)
	}
	if err := gitEnsure(ctx, reg.PlatformRoot, reg.PlatformRepo); err != nil {
		fatalf("clone platform repo: %v", redact(err.Error()))
	}
	k := kube.New(os.Getenv("KUBE_CONTEXT")) // empty => in-cluster / current context
	b := builder.New(k)
	b.Out = os.Stdout

	interval := time.Duration(reg.IntervalSeconds) * time.Second
	if interval < 15*time.Second {
		interval = 45 * time.Second
	}
	logf("idp-shipper up: env=%s platform=%s@%s apps=%d interval=%s",
		reg.Env, reg.PlatformRepo, branch(reg), len(reg.Apps), interval)

	// Seed last-shipped from the committed umbrella so a restart does NOT rebuild
	// every app: if the umbrella already pins an app's images at its current head
	// SHA, it is already shipped.
	last := map[string]string{}
	for _, app := range reg.Apps {
		sha, err := githubHeadSHA(ctx, app.Repo, app.Branch, token)
		if err != nil {
			continue
		}
		comps, err := fetchComponents(ctx, app, sha, token)
		if err != nil {
			continue
		}
		if umbrellaHasImagesAtTag(reg, deriveBuildTargets(comps), short(sha)) {
			last[app.Name] = sha
			logf("[%s] umbrella already at %s — skipping initial build", app.Name, short(sha))
		}
	}

	for {
		for _, app := range reg.Apps {
			sha, err := githubHeadSHA(ctx, app.Repo, app.Branch, token)
			if err != nil {
				logf("[%s] poll error: %v", app.Name, redact(err.Error()))
				continue
			}
			if sha == last[app.Name] {
				continue
			}
			logf("[%s] new commit %s on %s — shipping", app.Name, short(sha), app.Branch)
			if err := shipApp(ctx, reg, app, sha, token, b); err != nil {
				logf("[%s] ship FAILED @ %s: %v", app.Name, short(sha), redact(err.Error()))
				continue
			}
			last[app.Name] = sha
			logf("[%s] shipped %s ✓", app.Name, short(sha))
		}
		time.Sleep(interval)
	}
}

// shipApp fetches the app's shopping lists at sha, builds the derived image set,
// then renders+commits+pushes.
func shipApp(ctx context.Context, reg *Registry, app AppSpec, sha, token string, b *builder.Builder) error {
	tag := short(sha)
	comps, err := fetchComponents(ctx, app, sha, token)
	if err != nil {
		return err
	}
	targets := deriveBuildTargets(comps)
	if len(targets) == 0 {
		return fmt.Errorf("no buildable images in %s shopping lists", app.Name)
	}
	// 1. Build each image (expensive; do it once, outside the push-retry loop).
	for _, t := range targets {
		image := t.Image + ":" + tag
		logf("[%s] building %s (context %q, submodules %v)", app.Name, image, t.Context, t.Submodules)
		if err := b.Build(ctx, builder.Spec{
			Repo: app.Repo, Ref: sha, Image: image,
			Context: t.Context, Dockerfile: t.Dockerfile, Submodules: t.Submodules,
		}); err != nil {
			return fmt.Errorf("build %s: %w", image, err)
		}
	}
	// 2. Render + commit + push, retrying on a concurrent-writer push rejection.
	return renderCommitPush(ctx, reg, app, comps, tag)
}

// fetchComponents downloads and parses every shopping list for the app at sha.
func fetchComponents(ctx context.Context, app AppSpec, sha, token string) ([]component, error) {
	out := make([]component, 0, len(app.DeployFiles))
	for _, df := range app.DeployFiles {
		raw, err := githubFile(ctx, app.Repo, df, sha, token)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", df, err)
		}
		tmp, err := writeTemp(raw)
		if err != nil {
			return nil, err
		}
		cfg, err := appconfig.Load(tmp) // raw — Expand() defaults each component via deploy.Build
		os.Remove(tmp)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", df, err)
		}
		// One shopping list may declare several components (api+scanner+ui+…);
		// Expand() yields one full App per component (a plain file → just itself),
		// so a single deployFile entry can ship a whole multi-component product.
		for _, a := range cfg.Expand() {
			out = append(out, component{file: df, cfg: a})
		}
	}
	return out, nil
}

// deriveBuildTargets folds the shopping lists into the unique set of images to
// build. Components sharing an image (e.g. api + scanner) collapse to one build;
// their build blocks merge (union of submodules, first non-default context/dockerfile).
func deriveBuildTargets(comps []component) []buildTarget {
	byImage := map[string]*buildTarget{}
	order := []string{}
	for _, c := range comps {
		img := c.cfg.Runtime.Image
		if img == "" {
			continue
		}
		t, ok := byImage[img]
		if !ok {
			t = &buildTarget{Image: img, Context: ".", Dockerfile: "Dockerfile"}
			byImage[img] = t
			order = append(order, img)
		}
		if c.cfg.Build.Context != "" {
			t.Context = c.cfg.Build.Context
		}
		if c.cfg.Build.Dockerfile != "" {
			t.Dockerfile = c.cfg.Build.Dockerfile
		}
		t.Submodules = mergeUnique(t.Submodules, c.cfg.Build.Submodules)
	}
	out := make([]buildTarget, 0, len(order))
	for _, img := range order {
		out = append(out, *byImage[img])
	}
	return out
}

// renderCommitPush renders the app's components onto the current remote tip and
// pushes. It self-heals: every attempt starts by hard-resetting the checkout to
// origin (discarding any leftover writes from a prior partial failure or a local
// commit a rejected push left behind), so there is no ff-only wedge and no stale
// half-shipped umbrella swept into a later commit.
func renderCommitPush(ctx context.Context, reg *Registry, app AppSpec, comps []component, tag string) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := syncToRemote(ctx, reg); err != nil {
			return fmt.Errorf("sync platform repo: %w", err)
		}
		if err := renderComponents(reg, comps, tag); err != nil {
			return err
		}
		if err := git(ctx, reg.PlatformRoot, "add", "-A"); err != nil {
			return err
		}
		if err := git(ctx, reg.PlatformRoot, "diff", "--cached", "--quiet"); err == nil {
			logf("  [%s] no umbrella change; skipping commit", app.Name)
			return nil
		}
		msg := fmt.Sprintf("ship(%s): %s @ %s", reg.Env, app.Name, tag)
		if err := git(ctx, reg.PlatformRoot,
			"-c", "user.name=idp-shipper", "-c", "user.email=idp-shipper@noreply",
			"commit", "-m", msg); err != nil {
			return err
		}
		if err := git(ctx, reg.PlatformRoot, "push", "origin", "HEAD:"+branch(reg)); err == nil {
			return nil
		} else {
			lastErr = err
			logf("  [%s] push rejected (attempt %d/3); re-syncing", app.Name, attempt)
		}
	}
	return fmt.Errorf("push failed after retries: %w", lastErr)
}

// renderComponents upserts each parsed component into the umbrella with image
// repo:tag (a fresh DeployTime stamp forces a rollout even on a same-tag re-ship).
func renderComponents(reg *Registry, comps []component, tag string) error {
	cluster, err := loadCluster(reg.PlatformRoot, reg.Env)
	if err != nil {
		return fmt.Errorf("load cluster: %w", err)
	}
	deployTime := time.Now().UTC().Format(time.RFC3339)
	for _, c := range comps {
		image := c.cfg.Runtime.Image + ":" + tag
		plan, err := deploy.Build(deploy.Request{
			App: c.cfg, Env: reg.Env, Image: image, DeployTime: deployTime,
			Cluster: cluster, Root: reg.PlatformRoot,
		})
		if err != nil {
			return fmt.Errorf("render %s: %w", c.file, err)
		}
		if _, err := plan.Result.UpsertApp(reg.PlatformRoot, cluster); err != nil {
			return fmt.Errorf("upsert %s: %w", c.file, err)
		}
	}
	return nil
}

// umbrellaHasImagesAtTag reports whether the committed umbrella already pins every
// target image at tag (used to seed last-shipped on startup so a restart skips
// rebuilding unchanged apps).
func umbrellaHasImagesAtTag(reg *Registry, targets []buildTarget, tag string) bool {
	if len(targets) == 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(reg.PlatformRoot, "clusters", reg.Env, "platform.yaml"))
	if err != nil {
		return false
	}
	text := string(data)
	for _, t := range targets {
		if !imageAtTag(text, t.Image, tag) {
			return false
		}
	}
	return true
}

// imageAtTag reports whether the umbrella has `repository: <image>` followed
// (within a few lines, same values block) by `tag: <tag>`.
func imageAtTag(text, image, tag string) bool {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.Contains(ln, "repository: "+image) {
			for j := i + 1; j < len(lines) && j <= i+3; j++ {
				if strings.Contains(lines[j], "tag: "+tag) {
					return true
				}
			}
		}
	}
	return false
}

// ---- GitHub API ----

func githubHeadSHA(ctx context.Context, repo, branch, token string) (string, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", repo, branch)
	var out struct {
		SHA string `json:"sha"`
	}
	if err := githubJSON(ctx, u, token, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("empty sha for %s@%s", repo, branch)
	}
	return out.SHA, nil
}

func githubFile(ctx context.Context, repo, path, ref, token string) ([]byte, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", repo, path, ref)
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := githubJSON(ctx, u, token, &out); err != nil {
		return nil, err
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unexpected encoding %q", path, out.Encoding)
	}
	clean := strings.NewReplacer("\n", "", "\r", "").Replace(out.Content)
	return base64.StdEncoding.DecodeString(clean)
}

func githubJSON(ctx context.Context, url, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---- git (exec, same pattern as the kube wrapper — no shell script) ----

// setupGitAuth wires git auth out-of-band so the token never appears in a command
// arg, in .git/config, or in logs: the username (x-access-token) rides in the
// remote URL and GIT_ASKPASS supplies the token (read from $GIT_TOKEN) as the
// password on demand.
func setupGitAuth() error {
	const path = "/tmp/git-askpass.sh"
	script := "#!/bin/sh\nexec printf '%s' \"$GIT_TOKEN\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return err
	}
	os.Setenv("GIT_ASKPASS", path)
	os.Setenv("GIT_TERMINAL_PROMPT", "0")
	return nil
}

// authURL carries only the username (not a secret); the token comes from askpass.
func authURL(repo string) string {
	return fmt.Sprintf("https://x-access-token@github.com/%s.git", repo)
}

func gitEnsure(ctx context.Context, root, repo string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return nil
	}
	return git(ctx, "", "clone", authURL(repo), root)
}

// syncToRemote makes the checkout exactly origin/<branch> with a clean tree —
// the self-healing primitive: it discards leftover writes and any local commit a
// rejected push left behind, so the shipper can never wedge on a diverged local.
func syncToRemote(ctx context.Context, reg *Registry) error {
	if err := git(ctx, reg.PlatformRoot, "fetch", "origin", branch(reg)); err != nil {
		return err
	}
	return git(ctx, reg.PlatformRoot, "reset", "--hard", "origin/"+branch(reg))
}

func git(ctx context.Context, dir string, args ...string) error {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(string(out)))
	}
	return nil
}

// ---- helpers ----

func loadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if r.PlatformRoot == "" {
		r.PlatformRoot = "/platform"
	}
	if r.Env == "" {
		r.Env = "dev"
	}
	return &r, nil
}

func loadCluster(root, env string) (*clusterenv.Config, error) {
	path := filepath.Join(root, "environments", env, "cluster.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return clusterenv.Load(path)
}

func writeTemp(b []byte) (string, error) {
	f, err := os.CreateTemp("", "deploy-*.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			a = append(a, s)
			seen[s] = true
		}
	}
	return a
}

func branch(reg *Registry) string {
	if reg.PlatformBranch != "" {
		return reg.PlatformBranch
	}
	return "main"
}

func redact(s string) string {
	if redactor != nil {
		return redactor.Replace(s)
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func logf(format string, a ...any)   { fmt.Printf(time.Now().Format("15:04:05")+" "+format+"\n", a...) }
func fatalf(format string, a ...any) { fmt.Printf("FATAL "+format+"\n", a...); os.Exit(1) }
