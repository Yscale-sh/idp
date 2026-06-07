// idp-shipper is the infra-owned, in-cluster orchestrator that realizes the
// platform's "push to master -> it deploys" contract. Developers only ever touch
// their deploy.yaml shopping list; the shipper holds ALL the orchestration:
//
//	for each registered app, every interval:
//	  sha := github head(app.repo, app.branch)
//	  if sha changed:
//	    build each image via the homelab image-builder  (internal/builder)
//	    render each component into the platform umbrella (internal/deploy)
//	    git commit + push the platform repo             -> Flux reconciles
//
// It reuses idpctl's own render core (no second source of truth) and the kube
// kubectl wrapper (no client-go), so the umbrella has a single writer: render.
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

// Registry is the infra-owned config (a ConfigMap) of apps to ship. This is the
// platform's orchestration knowledge; the developer's repo holds none of it.
type Registry struct {
	Env             string    `json:"env"`             // target environment (dev|prod)
	PlatformRepo    string    `json:"platformRepo"`    // org/name of the idp repo to render+push
	PlatformRoot    string    `json:"platformRoot"`    // checkout dir in the pod (holds clusters/, environments/)
	IntervalSeconds int       `json:"intervalSeconds"` // poll cadence
	Apps            []AppSpec `json:"apps"`
}

// AppSpec registers one application.
type AppSpec struct {
	Name        string        `json:"name"`        // logical app name (for logs)
	Repo        string        `json:"repo"`        // org/name to watch + build
	Branch      string        `json:"branch"`      // branch to track (e.g. master)
	DeployFiles []string      `json:"deployFiles"` // deploy.yaml paths within the repo
	Builds      []BuildTarget `json:"builds"`      // images to build (context/submodules are build detail)
}

// BuildTarget is one image to build from the app repo.
type BuildTarget struct {
	Image      string   `json:"image"`                // ghcr repo WITHOUT tag (shipper appends :sha)
	Context    string   `json:"context,omitempty"`    // build context subdir ("." default)
	Submodules []string `json:"submodules,omitempty"` // private submodules to init
}

func main() {
	regPath := envOr("REGISTRY_PATH", "/etc/idp-shipper/registry.yaml")
	token := os.Getenv("GIT_TOKEN") // GitHub token: read app repos + push platform repo
	if token == "" {
		fatalf("GIT_TOKEN is required (GitHub token with repo scope)")
	}
	reg, err := loadRegistry(regPath)
	if err != nil {
		fatalf("load registry %s: %v", regPath, err)
	}
	ctx := context.Background()
	if err := gitEnsure(ctx, reg.PlatformRoot, reg.PlatformRepo, token); err != nil {
		fatalf("clone platform repo: %v", err)
	}
	k := kube.New(os.Getenv("KUBE_CONTEXT")) // empty => in-cluster / current context
	b := builder.New(k)
	b.Out = os.Stdout

	interval := time.Duration(reg.IntervalSeconds) * time.Second
	if interval < 15*time.Second {
		interval = 45 * time.Second
	}
	logf("idp-shipper up: env=%s platform=%s apps=%d interval=%s",
		reg.Env, reg.PlatformRepo, len(reg.Apps), interval)

	last := map[string]string{}
	for {
		for _, app := range reg.Apps {
			sha, err := githubHeadSHA(ctx, app.Repo, app.Branch, token)
			if err != nil {
				logf("[%s] poll error: %v", app.Name, err)
				continue
			}
			if sha == last[app.Name] {
				continue
			}
			logf("[%s] new commit %s on %s — shipping", app.Name, short(sha), app.Branch)
			if err := shipApp(ctx, reg, app, sha, token, b); err != nil {
				logf("[%s] ship FAILED @ %s: %v", app.Name, short(sha), err)
				continue
			}
			last[app.Name] = sha
			logf("[%s] shipped %s ✓", app.Name, short(sha))
		}
		time.Sleep(interval)
	}
}

// shipApp builds every image for the app at sha, renders each component into the
// platform umbrella, and commits+pushes the result (Flux then reconciles).
func shipApp(ctx context.Context, reg *Registry, app AppSpec, sha, token string, b *builder.Builder) error {
	tag := short(sha)

	// Refresh the platform checkout so render upserts onto current HEAD.
	if err := gitPull(ctx, reg.PlatformRoot, token); err != nil {
		return fmt.Errorf("pull platform repo: %w", err)
	}

	// 1. Build each image via the homelab image-builder.
	for _, t := range app.Builds {
		image := t.Image + ":" + tag
		logf("[%s] building %s (context %q)", app.Name, image, ctxOr(t.Context))
		if err := b.Build(ctx, builder.Spec{
			Repo: app.Repo, Ref: sha, Image: image,
			Context: t.Context, Submodules: t.Submodules,
		}); err != nil {
			return fmt.Errorf("build %s: %w", image, err)
		}
	}

	// 2. Render each component into the umbrella with its freshly-built image:tag.
	cluster, err := loadCluster(reg.PlatformRoot, reg.Env)
	if err != nil {
		return fmt.Errorf("load cluster: %w", err)
	}
	for _, df := range app.DeployFiles {
		raw, err := githubFile(ctx, app.Repo, df, sha, token)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", df, err)
		}
		tmp, err := writeTemp(raw)
		if err != nil {
			return err
		}
		cfg, err := appconfig.LoadDefaulted(tmp)
		os.Remove(tmp)
		if err != nil {
			return fmt.Errorf("parse %s: %w", df, err)
		}
		image := cfg.Runtime.Image + ":" + tag
		plan, err := deploy.Build(deploy.Request{
			App: cfg, Env: reg.Env, Image: image, Cluster: cluster, Root: reg.PlatformRoot,
		})
		if err != nil {
			return fmt.Errorf("render %s: %w", df, err)
		}
		if _, err := plan.Result.UpsertApp(reg.PlatformRoot, cluster); err != nil {
			return fmt.Errorf("upsert %s: %w", df, err)
		}
	}

	// 3. Commit + push the platform repo; Flux reconciles from there.
	msg := fmt.Sprintf("ship(%s): %s @ %s", reg.Env, app.Name, tag)
	return gitCommitPush(ctx, reg.PlatformRoot, msg, token)
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
	resp, err := http.DefaultClient.Do(req)
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

func gitEnsure(ctx context.Context, root, repo, token string) error {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return nil
	}
	url := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, repo)
	return git(ctx, "", "clone", url, root)
}

func gitPull(ctx context.Context, root, token string) error {
	if err := git(ctx, root, "remote", "set-url", "origin", remoteURL(root, token)); err != nil {
		return err
	}
	return git(ctx, root, "pull", "--ff-only")
}

func gitCommitPush(ctx context.Context, root, msg, token string) error {
	if err := git(ctx, root, "add", "-A"); err != nil {
		return err
	}
	// Nothing staged => nothing changed (idempotent reship); not an error.
	if err := git(ctx, root, "diff", "--cached", "--quiet"); err == nil {
		logf("  (no umbrella change; skipping commit)")
		return nil
	}
	if err := git(ctx, root,
		"-c", "user.name=idp-shipper", "-c", "user.email=idp-shipper@noreply",
		"commit", "-m", msg); err != nil {
		return err
	}
	if err := git(ctx, root, "remote", "set-url", "origin", remoteURL(root, token)); err != nil {
		return err
	}
	return git(ctx, root, "push", "origin", "HEAD")
}

func remoteURL(root, token string) string {
	out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	repo := "JakeNesler/idp"
	if err == nil {
		if r := repoFromURL(string(out)); r != "" {
			repo = r
		}
	}
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, repo)
}

func git(ctx context.Context, dir string, args ...string) error {
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, string(out))
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
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func ctxOr(c string) string {
	if c == "" {
		return "."
	}
	return c
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func stripNewlines(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' && s[i] != '\r' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func repoFromURL(u string) string {
	const host = "github.com/"
	i := strings.Index(strings.TrimSpace(u), host)
	if i < 0 {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(u)[i+len(host):], ".git")
}

func logf(format string, a ...any)   { fmt.Printf(time.Now().Format("15:04:05")+" "+format+"\n", a...) }
func fatalf(format string, a ...any) { fmt.Printf("FATAL "+format+"\n", a...); os.Exit(1) }
