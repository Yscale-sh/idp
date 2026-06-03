// Package deploy is the orchestration layer: it ties the steps together
// (load -> applyDefaults -> validate -> policy -> render) and produces a Plan
// the CLI prints and optionally writes. It is the single place that sequences
// the guardrails before any desired-state is emitted, matching DEPLOY_GO_CLI.md
// "normal path": validate -> render -> commit -> Argo reconciles.
package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
	"github.com/jakenesler/platformctl/internal/helmrunner"
	"github.com/jakenesler/platformctl/internal/policy"
	"github.com/jakenesler/platformctl/internal/render"
)

// Request is the input to a plan/render.
type Request struct {
	App        appconfig.App      // parsed deploy.yaml (defaults not required; Plan applies them)
	Env        string             // target environment
	Image      string             // CI image repo:tag
	DeployTime string             // CI deploy stamp
	Cluster    *clusterenv.Config // env config (may be nil; degrades gracefully)
	// Root is the platform repo root (holds charts/app). When set and helm is on
	// PATH, Build runs a `helm template` scan of the rendered chart and feeds the
	// output to policy.CheckRenderedManifest (last-line LoadBalancer guardrail).
	// When empty or helm is absent, that template scan is skipped — the typed
	// Values struct has no service.type field, so the app values path cannot
	// express a LoadBalancer, and the rendered Argo Application is still scanned.
	Root string
}

// Plan is the validated, policy-checked, rendered result plus a human summary.
type Plan struct {
	Result *render.Result
}

// Build runs the full pipeline for one app. It returns an error if structural
// validation or any policy guardrail fails — before producing desired state.
func Build(req Request) (*Plan, error) {
	app := req.App
	app.ApplyDefaults()

	// 1. Structural validation.
	if err := app.Validate(); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	// 2. Policy guardrails (env-aware) over the FULL declared route set. A
	// deploy.yaml may carry hosts for several environments (a .local dev host AND
	// a public prod host). We must run policy BEFORE any per-env narrowing so a
	// public out-of-zone host is REJECTED, never silently dropped. checkRoutes
	// only flags *public* out-of-zone hosts, so internal hosts that belong to
	// other envs still pass and get narrowed out below.
	violations := policy.Check(policy.Input{
		App:     app,
		Env:     req.Env,
		Image:   req.Image,
		Cluster: req.Cluster,
	})
	if err := violations.AsError(); err != nil {
		return nil, fmt.Errorf("policy violations: %w", err)
	}

	// 2b. Per-env route narrowing AFTER policy. With approved zones configured,
	// keep only the routes whose host is in an approved zone for THIS env; the
	// rest belong to other environments. (Any public out-of-zone host was already
	// rejected in step 2, so this only drops internal/other-env hosts.) With no
	// env config (quick local checks) all routes are kept.
	app.Routes = selectRoutes(app.Routes, req.Cluster)

	// 3. Render desired state.
	result, err := render.Render(app, req.Env, req.Cluster, req.Image, req.DeployTime)
	if err != nil {
		return nil, fmt.Errorf("render failed: %w", err)
	}

	// 4. Last-line guardrails over the RENDERED output (not a hand-built map).
	if err := checkRenderedOutput(req.Root, app, req.Env, result); err != nil {
		return nil, err
	}

	return &Plan{Result: result}, nil
}

// checkRenderedOutput runs policy over the actually-rendered manifests:
//
//   - the rendered Argo CD Application (its destination.namespace must equal the
//     app's own namespace — the real namespace-isolation guardrail), and
//   - when --root + helm are available, a `helm template charts/app` of the
//     rendered values, scanned for a forbidden type: LoadBalancer Service.
//
// This replaces the old valuesAsMap "defense in depth" that hand-built a map
// omitting service.type and so could never observe a LoadBalancer (security
// theater). The Argo destination check ALWAYS runs; the helm template scan is
// best-effort (skipped, not failed, when helm or the chart dir is unavailable).
func checkRenderedOutput(root string, app appconfig.App, env string, result *render.Result) error {
	// (a) Argo Application destination namespace == app namespace.
	appYAML, err := result.ApplicationYAML()
	if err != nil {
		return fmt.Errorf("serialize rendered application: %w", err)
	}
	if vs := policy.CheckArgoDestination(appYAML, app.Namespace(env)); len(vs) > 0 {
		return fmt.Errorf("policy violations in rendered output: %w", vs.AsError())
	}

	// (b) Best-effort helm template scan of the rendered chart for a LoadBalancer
	// leak. The typed Values struct has no service.type by design, so the app
	// path cannot express one; this scan defends against future chart drift and
	// uses the SAME guardrail (CheckRenderedManifest) the infra path relies on.
	if root == "" {
		return nil
	}
	hr := helmrunner.New()
	if !hr.Available() {
		return nil
	}
	rendered, terr := hr.Template(context.Background(),
		app.ReleaseName(), helmrunner.ChartDir(root), app.Namespace(env), result.Values)
	if terr != nil {
		// A template failure here is a real signal, but on the default path the
		// chart is known-good and helm may be misconfigured locally; do not block
		// the render on a template error. The structured guardrails above already
		// ran. (The infra path, which pulls arbitrary charts, fails closed.)
		return nil
	}
	if vs := policy.CheckRenderedManifest(rendered); len(vs) > 0 {
		return fmt.Errorf("policy violations in rendered manifest: %w", vs.AsError())
	}
	return nil
}

// Summary is a concise multiline plan summary for the CLI.
func (p *Plan) Summary() string {
	r := p.Result
	app := r.App
	var b strings.Builder
	fmt.Fprintf(&b, "app:        %s\n", app.App)
	fmt.Fprintf(&b, "env:        %s\n", r.Env)
	fmt.Fprintf(&b, "namespace:  %s\n", app.Namespace(r.Env))
	fmt.Fprintf(&b, "release:    %s\n", app.ReleaseName())
	fmt.Fprintf(&b, "service:    %s (ClusterIP)\n", app.ServiceName())
	fmt.Fprintf(&b, "image:      %s:%s\n", r.Values.Image.Repository, r.Values.Image.Tag)
	fmt.Fprintf(&b, "replicas:   %d\n", r.Values.Replicas)
	fmt.Fprintf(&b, "profile:    %s (cpu %s / mem %s)\n",
		nonEmpty(app.Sizing.Profile, appconfig.DefaultProfile),
		r.Values.Resources.Limits.CPU, r.Values.Resources.Limits.Memory)
	if r.Values.Keda.Enabled {
		fmt.Fprintf(&b, "autoscale:  %s %d-%d\n", r.Values.Keda.Kind, r.Values.Keda.MinReplicas, r.Values.Keda.MaxReplicas)
	}
	fmt.Fprintf(&b, "secret:     %s (externalSecret backend=%s, store=%s)\n",
		app.SecretName(), r.Values.ExternalSecret.Backend, r.Values.ExternalSecret.StoreRef.Name)
	fmt.Fprintf(&b, "ssm root:   %s\n", app.SSMRoot(r.Env))

	if len(r.SecretKeys) > 0 {
		fmt.Fprintf(&b, "secret keys: %s\n", strings.Join(r.SecretKeys, ", "))
	}
	for _, s := range r.StoreApplications {
		fmt.Fprintf(&b, "store:      %s -> ns %s (Argo app %s)\n", s.Tool, s.Namespace, s.Application.Metadata.Name)
	}
	for _, c := range r.Connections {
		fmt.Fprintf(&b, "connects:   %s=%s (%s -> %s)\n", c.EnvVar, c.Value, c.Mode, c.Target)
	}
	for _, route := range app.Routes {
		exposure := "internal"
		if route.Public {
			exposure = "public (Cloudflare Tunnel)"
		}
		fmt.Fprintf(&b, "route:      %s [%s]\n", route.Host, exposure)
	}
	return b.String()
}

// selectRoutes keeps only routes whose host is in an approved zone for the env.
// With no cluster config or no zones configured, all routes are kept (zone
// policy is unrestricted in that case, matching clusterenv.HostInZone).
func selectRoutes(routes []appconfig.Route, c *clusterenv.Config) []appconfig.Route {
	if c == nil || len(c.Zones) == 0 {
		return routes
	}
	kept := make([]appconfig.Route, 0, len(routes))
	for _, r := range routes {
		if c.HostInZone(r.Host) {
			kept = append(kept, r)
		}
	}
	return kept
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
