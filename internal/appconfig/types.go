// Package appconfig defines the deploy.yaml contract: the small "developer
// shopping list" a repo submits to platformctl. These types are the load-bearing
// interface every other package (render, policy, secrets, scaffold) implements
// against, so they are deliberately minimal, stable, and dependency-free.
//
// The flow:
//
//	deploy.yaml -> appconfig.Load+ApplyDefaults -> policy.Validate
//	            -> render charts/app values -> environments/<env>/apps/<app>.yaml
//	            -> Flux reconciles.
//
// Authoritative spec: DEPLOY_GO_CLI.md ("App contract") and ENV.md (env tiers).
//
// YAML note: callers unmarshal with sigs.k8s.io/yaml (JSON tags via struct json
// tags). We therefore declare BOTH `json` and `yaml` tags so the structs work
// whether decoded through sigs.k8s.io/yaml (json tags) or gopkg.in/yaml.v3 (yaml
// tags). This package itself imports nothing beyond the standard library.
package appconfig

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Top-level contract
// ---------------------------------------------------------------------------

// App is the entire deploy.yaml a developer authors. Everything not set here is
// derived by the platform (namespaces, secret names, SSM paths, env injection).
type App struct {
	// App is the stable handle for this workload. It derives the namespace,
	// Helm release, Service name, and SSM root. Required. DNS-1123 label.
	App string `json:"app" yaml:"app"`

	// Product groups related apps (e.g. an API + UI pair) for inventory and
	// cleanup. Optional. Renders label platform/product.
	Product string `json:"product,omitempty" yaml:"product,omitempty"`

	// Component distinguishes parts of a product (e.g. "api", "ui"). Optional.
	// Renders label platform/component.
	Component string `json:"component,omitempty" yaml:"component,omitempty"`

	// Runtime is the container + port. Required.
	Runtime Runtime `json:"runtime" yaml:"runtime"`

	// Routes are the hostnames this app answers on. Apps are ClusterIP only;
	// public exposure is via Cloudflare Tunnel (prod) or a local LAN expose
	// (on-prem), never an unmanaged LoadBalancer.
	Routes []Route `json:"routes,omitempty" yaml:"routes,omitempty"`

	// Env is arbitrary NON-SECRET plain env injected into the container (e.g.
	// DIM_ROLE=scanner, RUST_LOG=info). Secret values never go here — use the
	// secrets backend. Reserved Tier-A keys (PORT, ENVIRONMENT, ...) are ignored.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`

	// Volumes are extra pod volumes + their mounts (NFS, emptyDir, PVC) — e.g. a
	// read-only NFS media share, a shared-RW metadata dir, an ephemeral cache.
	Volumes []Volume `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// Expose opts an app into LOCAL LAN exposure (a MetalLB LoadBalancer) for the
	// on-prem backend where there is no Cloudflare Tunnel. Off unless set; the
	// no-LB guardrail still blocks any other LoadBalancer.
	Expose *Expose `json:"expose,omitempty" yaml:"expose,omitempty"`

	// Sizing controls replicas, resource profile, and autoscaling.
	Sizing Sizing `json:"sizing,omitempty" yaml:"sizing,omitempty"`

	// DB lists requested databases. First/default entry also yields DATABASE_URL.
	DB []DataStore `json:"db,omitempty" yaml:"db,omitempty"`

	// Cache lists requested caches. First/default entry also yields REDIS_URL.
	Cache []DataStore `json:"cache,omitempty" yaml:"cache,omitempty"`

	// Storage lists object-storage buckets (R2/S3-compatible).
	Storage []Storage `json:"storage,omitempty" yaml:"storage,omitempty"`

	// Logging toggles Loki/stdout wiring. Defaults enabled.
	Logging Logging `json:"logging,omitempty" yaml:"logging,omitempty"`

	// Metrics toggles ServiceMonitor / OTEL metrics wiring. Defaults enabled.
	Metrics Metrics `json:"metrics,omitempty" yaml:"metrics,omitempty"`

	// ConnectsTo declares app-to-app connections; the platform resolves the
	// correct address (cluster DNS vs public route) per environment.
	ConnectsTo []Connection `json:"connectsTo,omitempty" yaml:"connectsTo,omitempty"`

	// TailscaleEgress injects Tailscale egress sidecar env only when true (e.g.
	// reaching an out-of-cluster DB). Most apps leave this false once in-cluster.
	TailscaleEgress bool `json:"tailscaleEgress,omitempty" yaml:"tailscaleEgress,omitempty"`
}

// Runtime is the container image (without tag — CI supplies --image) and port.
type Runtime struct {
	// Image is the container repository, e.g. ghcr.io/jakenesler/<app>. The
	// concrete tag is injected by CI via --image, not stored in deploy.yaml.
	Image string `json:"image" yaml:"image"`

	// Port is the container port the app listens on. Drives PORT env + probes.
	Port int `json:"port" yaml:"port"`
}

// Route is one hostname plus its access policy.
type Route struct {
	// Host is the external hostname (e.g. carshowdb.example.com). Must be inside
	// an approved zone for the environment (policy guardrail).
	Host string `json:"host" yaml:"host"`

	// Public exposes the host via Cloudflare Tunnel + DNS. When false the route
	// is internal/cluster-only.
	Public bool `json:"public,omitempty" yaml:"public,omitempty"`

	// Access is the Cloudflare Access policy for this host.
	Access Access `json:"access,omitempty" yaml:"access,omitempty"`
}

// Access expresses who/what may reach a route through Cloudflare Access.
type Access struct {
	// Humans gates the route behind interactive human SSO (Cloudflare Access).
	Humans bool `json:"humans,omitempty" yaml:"humans,omitempty"`

	// ServiceToken issues a Cloudflare Access service token for machine callers.
	ServiceToken bool `json:"serviceToken,omitempty" yaml:"serviceToken,omitempty"`
}

// Sizing is the replica/resource/autoscale envelope.
type Sizing struct {
	// Profile selects a resource preset (minimal|small|medium|large). The
	// renderer maps profile -> resources.requests/limits.
	Profile string `json:"profile,omitempty" yaml:"profile,omitempty"`

	// Replicas is the desired baseline replica count (when not autoscaling, or
	// the autoscale floor seed).
	Replicas int `json:"replicas,omitempty" yaml:"replicas,omitempty"`

	// Autoscale configures KEDA/HPA scaling.
	Autoscale Autoscale `json:"autoscale,omitempty" yaml:"autoscale,omitempty"`

	// ExtraLimits are extended resource limits beyond the profile's cpu/memory —
	// e.g. {"gpu.intel.com/i915": "1"} for hardware (VAAPI/QSV) transcode. Merged
	// into the container's resources.limits.
	ExtraLimits map[string]string `json:"extraLimits,omitempty" yaml:"extraLimits,omitempty"`
}

// Autoscale configures KEDA-driven scaling. Kind+Triggers let an app pick either
// a standard keda.sh ScaledObject (cpu/memory/cron) or an http.keda.sh
// HTTPScaledObject (request-rate, scale-to-zero) — see CONVENTIONS.md.
type Autoscale struct {
	// Enabled turns autoscaling on.
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Min is the minimum replica count (0 enables scale-to-zero with HTTP).
	Min int `json:"min,omitempty" yaml:"min,omitempty"`

	// Max is the maximum replica count.
	Max int `json:"max,omitempty" yaml:"max,omitempty"`

	// Kind selects the KEDA object kind: "ScaledObject" (default) or
	// "HTTPScaledObject". Empty means the renderer chooses ScaledObject.
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`

	// ScaleToZero makes the app scale ALL THE WAY DOWN to 0 replicas when idle
	// and wake on the first request via the KEDA HTTP add-on interceptor. Sugar
	// for kind: HTTPScaledObject + min: 0 (authoritative — overrides both). The
	// first request after idle eats a cold start, and the app's route must flow
	// through the interceptor. Best for spiky / rarely-hit apps; the DB does NOT
	// scale to zero. Requires the keda-http-add-on module enabled in the env.
	ScaleToZero bool `json:"scaleToZero,omitempty" yaml:"scaleToZero,omitempty"`

	// Metric/Target are a convenience for the common single-trigger case, e.g.
	// Metric "cpu" Target "70". Triggers (below) override for advanced cases.
	Metric string `json:"metric,omitempty" yaml:"metric,omitempty"`
	Target string `json:"target,omitempty" yaml:"target,omitempty"`

	// Triggers are raw KEDA triggers passed through to the ScaledObject. Each is
	// a free-form map ({type, metadata}) so cpu/memory/cron/prometheus all work
	// without this contract enumerating every trigger source.
	Triggers []map[string]any `json:"triggers,omitempty" yaml:"triggers,omitempty"`
}

// DataStore is a requested database or cache (postgres, redis, etc.).
type DataStore struct {
	// Name is the stable handle. It becomes the env-var prefix: "primary" ->
	// PRIMARY_DATABASE_URL, "sessions" -> SESSIONS_REDIS_URL. The first/default
	// entry of each class also gets the bare DATABASE_URL / REDIS_URL alias.
	Name string `json:"name" yaml:"name"`

	// Type is the engine: postgres|redis|mongo (mongo is an external exception).
	Type string `json:"type" yaml:"type"`

	// Size selects the data-store resource preset (minimal|small|medium|large).
	Size string `json:"size,omitempty" yaml:"size,omitempty"`

	// Provision controls whether the platform STANDS UP this store. Default true.
	// Set false to SHARE a sibling component's store: the *_URL env is still wired
	// to the same app-level store, but no new store is provisioned. This is how two
	// components of one app (e.g. dim api + scanner) share one Postgres/Redis.
	Provision *bool `json:"provision,omitempty" yaml:"provision,omitempty"`
}

// Provisioned reports whether this store should be stood up by the platform
// (default true; false shares a sibling component's app-level store).
func (d DataStore) Provisioned() bool { return d.Provision == nil || *d.Provision }

// Storage is one object-storage bucket (R2 / S3-compatible).
type Storage struct {
	// Name is the stable handle and env-var prefix: "uploads" -> UPLOADS_BUCKET,
	// UPLOADS_ENDPOINT, UPLOADS_ACCESS_KEY_ID, UPLOADS_SECRET_ACCESS_KEY.
	Name string `json:"name" yaml:"name"`

	// Type is the provider: r2|s3 (one S3-compatible convention, default R2).
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// Bucket is the bucket name.
	Bucket string `json:"bucket,omitempty" yaml:"bucket,omitempty"`

	// Public marks the bucket as publicly readable (drives URL/base config).
	Public bool `json:"public,omitempty" yaml:"public,omitempty"`
}

// Volume is one extra pod volume plus where it mounts. Type selects the source:
//
//	nfs      an NFS export (server + path) — shared media / metadata across pods.
//	emptyDir an ephemeral per-pod scratch dir (e.g. a transcode cache).
//	pvc      an existing PersistentVolumeClaim (claim name).
type Volume struct {
	// Name is the volume handle (DNS-1123); referenced by the mount.
	Name string `json:"name" yaml:"name"`

	// Type is the source kind: nfs | emptyDir | pvc.
	Type string `json:"type" yaml:"type"`

	// MountPath is where the volume mounts in the container.
	MountPath string `json:"mountPath" yaml:"mountPath"`

	// ReadOnly mounts the volume read-only (e.g. a media library).
	ReadOnly bool `json:"readOnly,omitempty" yaml:"readOnly,omitempty"`

	// SubPath mounts a sub-directory of the source instead of its root.
	SubPath string `json:"subPath,omitempty" yaml:"subPath,omitempty"`

	// Server/Path are the NFS export (type: nfs).
	Server string `json:"server,omitempty" yaml:"server,omitempty"`
	Path   string `json:"path,omitempty" yaml:"path,omitempty"`

	// Claim is the PVC name (type: pvc).
	Claim string `json:"claim,omitempty" yaml:"claim,omitempty"`
}

// Expose opts an app into LOCAL LAN exposure on the on-prem backend: a MetalLB
// LoadBalancer in front of the app's Service, since the LAN has no Cloudflare
// Tunnel. This is the ONLY sanctioned LoadBalancer — the no-LB guardrail still
// blocks any other. IP pins a MetalLB address (else MetalLB auto-assigns).
type Expose struct {
	// LAN turns on the MetalLB LoadBalancer for this app (on-prem only).
	LAN bool `json:"lan,omitempty" yaml:"lan,omitempty"`

	// IP pins the MetalLB LoadBalancer IP (optional; else auto-assigned).
	IP string `json:"ip,omitempty" yaml:"ip,omitempty"`

	// Port is the LoadBalancer port (defaults to runtime.port).
	Port int `json:"port,omitempty" yaml:"port,omitempty"`
}

// Logging toggles platform logging wiring (Loki endpoint + app labels).
type Logging struct {
	// Enabled defaults to true via ApplyDefaults.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// Metrics toggles platform metrics wiring (ServiceMonitor / OTEL).
type Metrics struct {
	// Enabled defaults to true via ApplyDefaults.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Path overrides the scrape path (default /metrics).
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// Connection declares an app-to-app dependency. The platform resolves the target
// address per environment and injects it under Env.
type Connection struct {
	// App is the target app name (cross-repo connection). Mutually informative
	// with Component (intra-product connection).
	App string `json:"app,omitempty" yaml:"app,omitempty"`

	// Component is the target component within the same product.
	Component string `json:"component,omitempty" yaml:"component,omitempty"`

	// Env is the env-var name to inject the resolved address into, e.g.
	// API_BASE_URL.
	Env string `json:"env" yaml:"env"`

	// Port is the TARGET's listening port for a clusterService connection (the
	// target lives in another file, so its port is not otherwise known here). When
	// 0, the source app's port is used as a best-effort convention.
	Port int `json:"port,omitempty" yaml:"port,omitempty"`

	// Mode chooses how the address is resolved:
	//   publicRoute    -> target's external route (browser-facing)
	//   clusterService -> target's in-cluster Service DNS (server-to-server)
	//   serviceToken   -> publicRoute plus Cloudflare Access service-token refs
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

// Default sizing/profile/store/storage values. Centralized so the renderer,
// schema, and docs agree.
const (
	DefaultProfile      = "minimal"
	DefaultReplicas     = 1
	DefaultDBType       = "postgres"
	DefaultCacheType    = "redis"
	DefaultStorageType  = "r2"
	DefaultStoreSize    = "minimal"
	DefaultMetricsPath  = "/metrics"
	DefaultAutoscaleK   = "ScaledObject"
	HTTPScaledObjectK   = "HTTPScaledObject"
	DefaultConnectsMode = "clusterService"
)

// ValidProfiles is the closed set of resource presets the renderer understands.
var ValidProfiles = []string{"minimal", "small", "medium", "large"}

// Defaults returns a fresh App pre-populated with the conventional defaults.
// Useful for scaffolding (`platformctl new app`).
func Defaults() App {
	a := App{}
	a.ApplyDefaults()
	return a
}

func boolPtr(b bool) *bool { return &b }

// ApplyDefaults fills in conventional defaults in place. It is idempotent: it
// only sets fields that are empty/unset, so an explicit value is never
// overwritten. Call this after loading deploy.yaml and before validation/render.
func (a *App) ApplyDefaults() {
	// Sizing.
	if a.Sizing.Profile == "" {
		a.Sizing.Profile = DefaultProfile
	}
	if a.Sizing.Replicas == 0 {
		a.Sizing.Replicas = DefaultReplicas
	}
	if a.Sizing.Autoscale.Enabled {
		if a.Sizing.Autoscale.Kind == "" {
			a.Sizing.Autoscale.Kind = DefaultAutoscaleK
		}
		if a.Sizing.Autoscale.Min == 0 && a.Sizing.Autoscale.Kind != HTTPScaledObjectK {
			a.Sizing.Autoscale.Min = a.Sizing.Replicas
		}
		if a.Sizing.Autoscale.Max == 0 {
			a.Sizing.Autoscale.Max = a.Sizing.Autoscale.Min
			if a.Sizing.Autoscale.Max == 0 {
				a.Sizing.Autoscale.Max = a.Sizing.Replicas
			}
		}
	}

	// Data stores.
	for i := range a.DB {
		if a.DB[i].Type == "" {
			a.DB[i].Type = DefaultDBType
		}
		if a.DB[i].Size == "" {
			a.DB[i].Size = DefaultStoreSize
		}
	}
	for i := range a.Cache {
		if a.Cache[i].Type == "" {
			a.Cache[i].Type = DefaultCacheType
		}
		if a.Cache[i].Size == "" {
			a.Cache[i].Size = DefaultStoreSize
		}
	}
	for i := range a.Storage {
		if a.Storage[i].Type == "" {
			a.Storage[i].Type = DefaultStorageType
		}
	}

	// Connections.
	for i := range a.ConnectsTo {
		if a.ConnectsTo[i].Mode == "" {
			a.ConnectsTo[i].Mode = DefaultConnectsMode
		}
	}

	// Observability defaults to on.
	if a.Logging.Enabled == nil {
		a.Logging.Enabled = boolPtr(true)
	}
	if a.Metrics.Enabled == nil {
		a.Metrics.Enabled = boolPtr(true)
	}
	if a.Metrics.Path == "" {
		a.Metrics.Path = DefaultMetricsPath
	}
}

// LoggingEnabled reports the effective logging toggle (true by default).
func (a *App) LoggingEnabled() bool {
	return a.Logging.Enabled == nil || *a.Logging.Enabled
}

// MetricsEnabled reports the effective metrics toggle (true by default).
func (a *App) MetricsEnabled() bool {
	return a.Metrics.Enabled == nil || *a.Metrics.Enabled
}

// ---------------------------------------------------------------------------
// Naming derivation (single source of truth for derived names)
// ---------------------------------------------------------------------------

// DefaultComponent is the workload "purpose" used when deploy.yaml omits
// component. Every rendered workload gets its own namespace named
// <app>-<env>-<purpose>; an app with no component is purpose "app".
const DefaultComponent = "app"

// Purpose returns the workload's purpose: the deploy.yaml component, or
// DefaultComponent ("app") when unset. It is the {purpose} segment of the app's
// namespace (<app>-<env>-<purpose>). (Named Purpose, not Component, because the
// App.Component field already occupies that identifier.)
func (a *App) Purpose() string {
	if a.Component != "" {
		return a.Component
	}
	return DefaultComponent
}

// Namespace is the Kubernetes namespace for the app workload:
// <app>-<env>-<purpose>, where purpose is the app's component (default "app").
// Every rendered workload gets its OWN namespace, created from the rendered YAML
// (the Flux HelmRelease sets targetNamespace + install.createNamespace=true).
func (a *App) Namespace(env string) string {
	return SanitizeDNSLabel(a.App + "-" + env + "-" + a.Purpose())
}

// StoreNamespace is the dedicated namespace for a declared data store:
// <app>-<env>-<tool> (e.g. carshowdb-dev-postgres, anyrent-dev-redis). A second
// store of the same tool disambiguates with the store name: <app>-<env>-<tool>-<name>.
// tool is the store's engine "purpose" (e.g. "postgres", "redis"); when secondary
// is true the store name is appended so two stores of the same tool never collide.
func (a *App) StoreNamespace(env, tool, name string, secondary bool) string {
	base := a.App + "-" + env + "-" + tool
	if secondary && name != "" {
		base += "-" + name
	}
	return SanitizeDNSLabel(base)
}

// SanitizeDNSLabel lowercases s and reduces it to a valid RFC1123 DNS label:
// only [a-z0-9-], no leading/trailing/double dashes, max 63 chars. Any other
// rune becomes a dash. It is the single sanitizer for every platform-derived
// namespace so the create-from-yaml namespaces are always apply-able.
func SanitizeDNSLabel(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// Workload is the unique per-workload handle: <app>-<component> when a component
// is set, else <app>. It names the HelmRelease, ClusterIP Service, runtime Secret,
// and the umbrella key — so multiple components of ONE app (e.g. dim's api /
// scanner / ui) never collide. The SSM root stays app-level (see SSMRoot) so
// sibling components SHARE secrets. For a single-component app it is just <app>.
func (a *App) Workload() string {
	if a.Component != "" {
		return SanitizeDNSLabel(a.App + "-" + a.Component)
	}
	return a.App
}

// IsWorker reports whether this is a portless worker (runtime.port == 0): the
// platform renders a Deployment only — no Service, no route, no HTTP probes, no
// ServiceMonitor. For background processors like a media scanner.
func (a *App) IsWorker() bool { return a.Runtime.Port == 0 }

// ReleaseName is the Helm release name: the workload handle.
func (a *App) ReleaseName() string { return a.Workload() }

// ServiceName is the ClusterIP Service name: the workload handle.
func (a *App) ServiceName() string { return a.Workload() }

// SecretName is the runtime Secret name materialized by ExternalSecret:
// <workload>-runtime.
func (a *App) SecretName() string { return a.Workload() + "-runtime" }

// ReleaseHandle is the Flux HelmRelease name (and Helm release name): the handle.
func (a *App) ReleaseHandle() string { return a.Workload() }

// SSMRoot returns the per-env SSM app root: /apps/<app>/<env>.
func (a *App) SSMRoot(env string) string {
	return fmt.Sprintf("/apps/%s/%s", a.App, env)
}

// SSMCapabilityPath returns the SSM path for a named capability resource:
// /apps/<app>/<env>/<capability>/<name>.
func (a *App) SSMCapabilityPath(env, capability, name string) string {
	return fmt.Sprintf("/apps/%s/%s/%s/%s", a.App, env, capability, name)
}

// EnvPrefix converts a capability name to a SCREAMING_SNAKE_CASE env-var prefix.
// "publicAssets" -> "PUBLIC_ASSETS"; "private-uploads" -> "PRIVATE_UPLOADS".
func EnvPrefix(name string) string {
	var b strings.Builder
	prevLower := false
	for i, r := range name {
		switch {
		case r == '-' || r == '_' || r == ' ':
			b.WriteByte('_')
			prevLower = false
		case r >= 'A' && r <= 'Z':
			if i > 0 && prevLower {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevLower = false
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
			prevLower = true
		default: // digits and others pass through
			b.WriteRune(r)
			prevLower = false
		}
	}
	// Collapse any accidental double underscores.
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}

// ---------------------------------------------------------------------------
// Env-aware view
// ---------------------------------------------------------------------------

// Standard environment tier names. Apps may target any tier; the platform maps
// a tier to a concrete environment directory under environments/<env>/.
const (
	EnvDev   = "dev"
	EnvProd  = "prod"
	EnvLocal = "local"
)

// Resolved is an env-aware, fully-defaulted projection of an App for a target
// environment. The renderer builds it; downstream steps read it instead of
// re-deriving names. It deliberately carries only resolved values, not policy.
type Resolved struct {
	// Env is the target environment name (e.g. "dev", "prod").
	Env string

	// App is the (defaults-applied) source contract.
	App App

	// Namespace/Release/Service/Secret are the derived names for this app.
	Namespace string
	Release   string
	Service   string
	Secret    string

	// SSMRoot is the per-env SSM root for this app.
	SSMRoot string

	// Labels is the full platform label set to stamp on every generated object.
	Labels map[string]string
}

// Resolve produces an env-aware view, applying defaults first. The returned
// Resolved is what the renderer and secrets steps consume.
func (a App) Resolve(env string) Resolved {
	a.ApplyDefaults()
	return Resolved{
		Env:       env,
		App:       a,
		Namespace: a.Namespace(env),
		Release:   a.ReleaseName(),
		Service:   a.ServiceName(),
		Secret:    a.SecretName(),
		SSMRoot:   a.SSMRoot(env),
		Labels:    a.Labels(env),
	}
}

// Labels returns the full platform label set for generated resources. These map
// directly onto the values.yaml `platform.*` keys and the chart's label helper.
func (a *App) Labels(env string) map[string]string {
	l := map[string]string{
		"app.kubernetes.io/name":     a.App,
		"app.kubernetes.io/instance": a.App,
		"platform/app":               a.App,
		"platform/env":               env,
		"platform/managed-by":        "platformctl",
	}
	if a.Product != "" {
		l["platform/product"] = a.Product
	}
	if a.Component != "" {
		l["platform/component"] = a.Component
	}
	return l
}
