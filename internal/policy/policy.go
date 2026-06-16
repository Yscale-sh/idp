// Package policy enforces the platform guardrails from CONVENTIONS.md §6 and
// DEPLOY_GO_CLI.md "Guardrails". These are checked BEFORE platformctl renders or
// mutates anything, and they surface as explicit typed errors — never buried
// Helm failures.
//
// The guardrails:
//  1. A rendered Service must be ClusterIP (never LoadBalancer).
//  2. In prod, the image tag must be immutable (never "latest", never empty).
//  3. A route host must be inside the env's approved zones.
//  4. Resources/profile must be valid and within env bounds.
//  5. An app must deploy into its own namespace (<app>), not another's.
//
// Each violation is a typed error (errors.Is-friendly via sentinel Kind) so
// callers and tests can assert on the specific rule that fired.
package policy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

// Kind identifies which guardrail a Violation tripped.
type Kind string

const (
	KindLoadBalancer   Kind = "LoadBalancer"
	KindMutableTag     Kind = "MutableTag"
	KindRouteZone      Kind = "RouteZone"
	KindResourceBounds Kind = "ResourceBounds"
	KindNamespace      Kind = "Namespace"
	KindInvalidProfile Kind = "InvalidProfile"
	KindMissingSecrets Kind = "MissingSecrets"
	KindUnprovidedSeam Kind = "UnprovidedSeam"
)

// Sentinel errors so callers can errors.Is on a Kind without constructing a
// Violation.
var (
	ErrLoadBalancer   = errors.New("policy: Service must be ClusterIP, not LoadBalancer")
	ErrMutableTag     = errors.New("policy: image tag must be immutable in prod (not 'latest' or empty)")
	ErrRouteZone      = errors.New("policy: route host outside approved zones")
	ErrResourceBounds = errors.New("policy: resources outside allowed bounds")
	ErrNamespace      = errors.New("policy: app must deploy into its own namespace")
	ErrInvalidProfile = errors.New("policy: invalid resource profile")
)

// Violation is a single guardrail failure. It wraps a sentinel error keyed by
// Kind, so errors.Is(v, ErrMutableTag) works.
type Violation struct {
	Kind    Kind
	Message string
}

func (v *Violation) Error() string { return fmt.Sprintf("[%s] %s", v.Kind, v.Message) }

func (v *Violation) Unwrap() error {
	switch v.Kind {
	case KindLoadBalancer:
		return ErrLoadBalancer
	case KindMutableTag:
		return ErrMutableTag
	case KindRouteZone:
		return ErrRouteZone
	case KindResourceBounds:
		return ErrResourceBounds
	case KindNamespace:
		return ErrNamespace
	case KindInvalidProfile:
		return ErrInvalidProfile
	case KindMissingSecrets:
		return ErrMissingSecrets
	}
	return nil
}

// ErrMissingSecrets is reported when a required secret store reference is absent.
var ErrMissingSecrets = errors.New("policy: required secret store reference missing")

// Violations is a set of guardrail failures. It is an error when non-empty.
type Violations []*Violation

func (vs Violations) Error() string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.Error()
	}
	return strings.Join(parts, "; ")
}

// AsError returns vs as an error (nil when empty).
func (vs Violations) AsError() error {
	if len(vs) == 0 {
		return nil
	}
	return vs
}

// Unwrap exposes the underlying violations as a multi-error so errors.Is/As walk
// into each *Violation (which itself unwraps to a sentinel ErrXxx by Kind). This
// lets callers do errors.Is(vs, ErrMutableTag) on the whole set.
func (vs Violations) Unwrap() []error {
	errs := make([]error, len(vs))
	for i, v := range vs {
		errs[i] = v
	}
	return errs
}

// Input is everything the guardrails inspect.
type Input struct {
	// App is the defaults-applied deploy contract.
	App appconfig.App
	// Env is the target environment name.
	Env string
	// Image is the fully-qualified image (repo:tag) CI injected via --image.
	Image string
	// Cluster is the env config (zones, bounds, store). May be nil for the
	// minimal checks that don't need it (those degrade gracefully).
	Cluster *clusterenv.Config
}

// Check runs every guardrail against the input and returns all violations.
//
// Routes are checked on the FULL set of declared routes (in.App.Routes) before
// any per-env narrowing the caller may do afterwards. A *public* route whose
// host is outside the env's approved zones is rejected here — it must never be
// silently dropped. Internal/non-public out-of-zone hosts are allowed (they
// belong to other environments and the caller narrows them out by zone).
func Check(in Input) Violations {
	var vs Violations
	app := in.App

	// (4) Resource profile must be valid.
	if app.Sizing.Profile != "" && !validProfile(app.Sizing.Profile) {
		vs = append(vs, &Violation{KindInvalidProfile,
			fmt.Sprintf("sizing.profile %q must be one of %s", app.Sizing.Profile, strings.Join(appconfig.ValidProfiles, "|"))})
	}

	// (2) Mutable tag: always rejected in prod; in non-prod rejected unless the
	// env explicitly opts in via allowMutableTags.
	if v := checkImageTag(in.Env, in.Image, in.Cluster); v != nil {
		vs = append(vs, v)
	}

	// (3) Route zones — checked over the full declared route set.
	vs = append(vs, checkRoutes(app, in.Cluster)...)

	// (4) Resource bounds (needs cluster bounds + chosen profile resources).
	vs = append(vs, checkResourceBounds(app, in.Cluster)...)

	// (5) Seam contract: an app may only request a capability the env provides.
	vs = append(vs, checkSeams(app, in.Cluster)...)

	return vs
}

// checkSeams enforces the loose-coupling contract from the app side: a
// deploy.yaml may only REQUEST a seam (data store, LAN exposure, public route,
// autoscaling, volumes) that the target env DECLARES it provides
// (clusterenv.EffectiveSeams). Fail-closed: an app can't quietly depend on a
// capability the cluster doesn't offer (e.g. an in-cluster Postgres in a
// PVC-free prod). Degrades to a no-op when the env config is absent.
func checkSeams(app appconfig.App, c *clusterenv.Config) Violations {
	if c == nil {
		return nil
	}
	s := c.EffectiveSeams()
	var vs Violations
	deny := func(req, seam, hint string) {
		vs = append(vs, &Violation{KindUnprovidedSeam,
			fmt.Sprintf("requests %s but env %q does not provide the %q seam — %s", req, c.Env, seam, hint)})
	}
	// db/cache is satisfiable EITHER in-cluster (statefulStores) OR by a managed
	// secrets backend (ssm) that supplies the connection URL: in a managed env the
	// renderer skips the in-cluster chart (render.BuildStoreReleases returns nil)
	// and leaves DATABASE_URL/REDIS_URL to come from the backend (render.buildStores
	// leaves the connection nil) — the same "declare once, the env fulfills it
	// differently" contract as routes (LAN in dev, tunnel in prod). Deny only when
	// the env can provide the store NEITHER way: no in-cluster stores AND no managed
	// backend (e.g. a local-backend env that opted out of statefulStores).
	managedStores := c.Secrets.Backend == clusterenv.BackendSSM
	if (len(app.DB) > 0 || len(app.Cache) > 0) && !s.StatefulStores && !managedStores {
		deny("an in-cluster data store (db/cache)", "statefulStores",
			"this env hosts no stores and has no managed secrets backend to supply DATABASE_URL/REDIS_URL")
	}
	if app.Expose != nil && app.Expose.LAN && !s.LANExpose {
		deny("expose.lan", "lanExpose", "no MetalLB here; apps are ClusterIP behind tunnels")
	}
	// A public route means "expose to users". The env fulfills that EITHER via a
	// Cloudflare Tunnel (publicRoutes) OR, on-prem, via a MetalLB LAN LoadBalancer
	// (lanExpose) — so the same deploy.yaml route works in both. Deny only when the
	// env provides neither path.
	if !s.PublicRoutes && !s.LANExpose {
		for _, r := range app.Routes {
			if r.Public {
				deny("a public route ("+r.Host+")", "publicRoutes/lanExpose",
					"this env exposes neither a Cloudflare Tunnel nor a LAN LoadBalancer")
				break
			}
		}
	}
	if (app.Sizing.Autoscale.Enabled || app.Sizing.Autosize) && !s.Autoscale {
		deny("sizing.autoscale/autosize", "autoscale", "no KEDA/VPA here to drive scaling")
	}
	if len(app.Volumes) > 0 && !s.Volumes {
		deny("volumes[]", "volumes", "this env mounts no volumes")
	}
	return vs
}

// CheckRenderedValues is the LoadBalancer guardrail at the rendered-output layer.
// The chart cannot express a LoadBalancer (no service.type key), but defense in
// depth: scan the rendered values for any service.type=LoadBalancer leak.
func CheckRenderedValues(values map[string]any) Violations {
	var vs Violations
	if svc, ok := values["service"].(map[string]any); ok {
		if t, ok := svc["type"].(string); ok && strings.EqualFold(t, "LoadBalancer") {
			vs = append(vs, &Violation{KindLoadBalancer,
				"rendered service.type is LoadBalancer; apps must be ClusterIP"})
		}
	}
	return vs
}

// CheckRenderedManifest scans an already-serialized manifest (Helm template
// output or Flux HelmRelease YAML) for a forbidden LoadBalancer Service. This is
// the last-line guardrail over arbitrary rendered YAML. It is wired into the app
// render path (helm-templated chart output) AND the infra/module path (templated
// module manifests), so a chart that emits `type: LoadBalancer` fails before any
// mutation.
func CheckRenderedManifest(manifest []byte) Violations {
	var vs Violations
	// Cheap textual pre-filter before structured parse.
	if !strings.Contains(string(manifest), "LoadBalancer") {
		return vs
	}
	// Walk every YAML doc; flag any Service whose spec.type is LoadBalancer.
	for _, doc := range splitYAMLDocs(manifest) {
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if kindOf(obj) != "Service" {
			continue
		}
		if spec, ok := obj["spec"].(map[string]any); ok {
			if t, ok := spec["type"].(string); ok && strings.EqualFold(t, "LoadBalancer") {
				// The ONE sanctioned LoadBalancer: the on-prem LAN expose (the app
				// chart's lan-service.yaml, label platform/expose=lan), an explicit,
				// renderer-gated opt-in for LANs with no Cloudflare Tunnel. Every
				// other LoadBalancer is still rejected.
				if isSanctionedLANExpose(obj) {
					continue
				}
				name := metaName(obj)
				vs = append(vs, &Violation{KindLoadBalancer,
					fmt.Sprintf("Service %q renders type LoadBalancer; apps must be ClusterIP", name)})
			}
		}
	}
	return vs
}

// isSanctionedLANExpose reports whether a LoadBalancer Service is the platform's
// explicit on-prem LAN expose — the app chart's lan-service.yaml, identified by
// the label platform/expose=lan. That template is renderer-gated (only set for an
// app that opts into expose.lan on a local backend), so this is the single
// LoadBalancer the guardrail permits; any other type:LoadBalancer is rejected.
func isSanctionedLANExpose(obj map[string]any) bool {
	meta, ok := obj["metadata"].(map[string]any)
	if !ok {
		return false
	}
	labels, ok := meta["labels"].(map[string]any)
	if !ok {
		return false
	}
	v, _ := labels["platform/expose"].(string)
	return v == "lan"
}

// CheckModuleValues scans an infra module's inline Helm values override for a
// forbidden LoadBalancer Service. A chartRepo module pulls its chart remotely
// (we cannot `helm template` it offline), but its inline values.valuesObject is
// the surface a platform author controls — a `service.type: LoadBalancer` (or
// any nested `type: LoadBalancer`) there is the realistic way a module would
// ship a LoadBalancer. This recursively walks the values tree and flags any map
// carrying a `type` of LoadBalancer. moduleName is used in the message.
func CheckModuleValues(moduleName string, values map[string]any) Violations {
	var vs Violations
	if hasLoadBalancerType(values) {
		vs = append(vs, &Violation{KindLoadBalancer,
			fmt.Sprintf("module %q inline values set a LoadBalancer service type; modules must not expose LoadBalancers", moduleName)})
	}
	return vs
}

// hasLoadBalancerType reports whether v (or any nested map/slice) contains a
// `type` key whose value is "LoadBalancer" (case-insensitive).
func hasLoadBalancerType(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["type"].(string); ok && strings.EqualFold(s, "LoadBalancer") {
			return true
		}
		for _, child := range t {
			if hasLoadBalancerType(child) {
				return true
			}
		}
	case []any:
		for _, child := range t {
			if hasLoadBalancerType(child) {
				return true
			}
		}
	}
	return false
}

// CheckHelmReleaseTarget validates the namespace-isolation guardrail on the
// RENDERED Flux HelmRelease: its spec.targetNamespace must equal the app's own
// namespace (<app>-<env>-<purpose>). One-namespace-per-app means a developer can
// never target another app's namespace; this is the real check. wantNamespace is
// the app's derived namespace.
func CheckHelmReleaseTarget(manifest []byte, wantNamespace string) Violations {
	var vs Violations
	for _, doc := range splitYAMLDocs(manifest) {
		var obj map[string]any
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			continue
		}
		if kindOf(obj) != "HelmRelease" {
			continue
		}
		spec, ok := obj["spec"].(map[string]any)
		if !ok {
			continue
		}
		got, _ := spec["targetNamespace"].(string)
		if got != wantNamespace {
			vs = append(vs, &Violation{KindNamespace,
				fmt.Sprintf("HelmRelease targetNamespace %q must equal app namespace %q",
					got, wantNamespace)})
		}
	}
	return vs
}

// checkImageTag enforces the immutable-tag guardrail. Prod ALWAYS rejects an
// empty or `latest` tag. A non-prod env also rejects them UNLESS its config
// opts in with allowMutableTags=true (the convenience for fast dev iteration).
// A nil cluster (quick local checks) is treated as "mutable allowed" off-prod.
func checkImageTag(env, image string, c *clusterenv.Config) *Violation {
	isProd := env == appconfig.EnvProd
	// Off-prod, only enforce when the env explicitly forbids mutable tags.
	if !isProd {
		if c == nil || c.AllowMutableTags {
			return nil
		}
	}
	tag := imageTag(image)
	where := env
	if tag == "" {
		return &Violation{KindMutableTag,
			fmt.Sprintf("%s image has no tag; CI must supply an immutable --image tag", where)}
	}
	if strings.EqualFold(tag, "latest") {
		return &Violation{KindMutableTag,
			fmt.Sprintf("%s image tag is 'latest' (mutable); use an immutable tag", where)}
	}
	return nil
}

// checkRoutes enforces the route-zone guardrail over the FULL declared route
// set. A route host outside the env's approved zones is a violation ONLY when
// the route is public (Cloudflare-Tunnel-exposed) — a public out-of-zone host
// must never be silently dropped. Internal/non-public hosts are allowed even
// when out of zone, because a single deploy.yaml may carry hosts for several
// environments and the caller narrows the surviving routes by zone afterwards.
func checkRoutes(app appconfig.App, c *clusterenv.Config) Violations {
	var vs Violations
	if c == nil {
		return vs
	}
	for _, r := range app.Routes {
		if r.Host == "" {
			continue
		}
		// Compose a bare label to this env's full host before the zone check, so a
		// single `host: web` (which becomes web.local / web.example.com per env) is
		// validated against the env it's rendering for, not rejected as out-of-zone.
		if r.Public && !c.HostInZone(c.ComposeHost(r.Host)) {
			vs = append(vs, &Violation{KindRouteZone,
				fmt.Sprintf("public route host %q is not in any approved zone for env %q (%s)",
					c.ComposeHost(r.Host), c.Env, strings.Join(c.Zones, ", "))})
		}
	}
	return vs
}

func checkResourceBounds(app appconfig.App, c *clusterenv.Config) Violations {
	var vs Violations
	if c == nil || c.ResourceBounds == nil {
		return vs
	}
	profile := app.Sizing.Profile
	if profile == "" {
		profile = appconfig.DefaultProfile
	}
	res, ok := ProfileResources(profile)
	if !ok {
		return vs // invalid profile already reported elsewhere.
	}
	b := c.ResourceBounds
	if b.MaxCPU != "" {
		if exceedsCPU(res.Limits.CPU, b.MaxCPU) {
			vs = append(vs, &Violation{KindResourceBounds,
				fmt.Sprintf("profile %q cpu limit %s exceeds env max %s", profile, res.Limits.CPU, b.MaxCPU)})
		}
	}
	if b.MaxMemory != "" {
		if exceedsMemory(res.Limits.Memory, b.MaxMemory) {
			vs = append(vs, &Violation{KindResourceBounds,
				fmt.Sprintf("profile %q memory limit %s exceeds env max %s", profile, res.Limits.Memory, b.MaxMemory)})
		}
	}
	return vs
}

func validProfile(p string) bool {
	for _, v := range appconfig.ValidProfiles {
		if v == p {
			return true
		}
	}
	return false
}
