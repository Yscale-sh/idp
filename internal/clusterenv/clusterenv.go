// Package clusterenv models environments/<env>/cluster.yaml: the per-environment
// configuration that the renderer, policy, secrets, and modules packages consume.
//
// One file per environment carries everything that is NOT in a developer's
// deploy.yaml: the secrets backend + SecretStore, the platform observability
// endpoints injected as Tier-A env, the approved route zones, the resource
// bounds for policy, and the module registry (platform infra to reconcile).
//
// This file is platform-authored (committed, OSS-clean: no secret values, only
// references and endpoints). It is the single source of env-specific truth.
package clusterenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Backend selects the secrets backend for an environment.
const (
	BackendLocal = "local" // dev/on-prem: external-secrets Kubernetes provider or plain Secret.
	BackendSSM   = "ssm"   // prod/cloud: AWS SSM Parameter Store via external-secrets.
)

// SecretStore kinds (external-secrets).
const (
	KindClusterSecretStore = "ClusterSecretStore"
	KindSecretStore        = "SecretStore"
)

// Module source kinds.
const (
	SourceLocalChart = "localChart" // chart lives in this repo under charts/.
	SourceChartRepo  = "chartRepo"  // chart pulled from a Helm repo or OCI registry.
)

// Config is the parsed environments/<env>/cluster.yaml.
type Config struct {
	// Env is the environment name (dev|prod|local|...). Defaults from the path.
	Env string `json:"env,omitempty"`

	// Secrets configures the per-env secrets backend and the SecretStore the app
	// chart's ExternalSecret references.
	Secrets SecretsConfig `json:"secrets,omitempty"`

	// Observability holds the platform endpoints injected into every app as
	// Tier-A env (LOKI_URL, OTEL_EXPORTER_OTLP_ENDPOINT, CONSOLE_LOGGING).
	Observability Observability `json:"observability,omitempty"`

	// Zones is the set of approved DNS zones (suffixes) a route host must fall
	// under. A route host outside every zone is a policy violation.
	Zones []string `json:"zones,omitempty"`

	// ResourceBounds caps per-app resource requests/limits for policy.
	ResourceBounds *ResourceBounds `json:"resourceBounds,omitempty"`

	// AllowMutableTags lets a non-prod env tolerate :latest. Prod is hard-coded
	// to reject it regardless of this flag.
	AllowMutableTags bool `json:"allowMutableTags,omitempty"`

	// Domain is the cluster-internal DNS domain for clusterService connections.
	// Defaults to "svc.cluster.local".
	Domain string `json:"domain,omitempty"`

	// Flux holds Flux wiring: the GitRepository source HelmReleases reference
	// cross-namespace (name + namespace), the repo URL the FluxInstance syncs
	// from, and the git branch it tracks. Defaults applied if unset.
	Flux FluxConfig `json:"flux,omitempty"`

	// Promotion declares how workloads ARRIVE in this env. Pure data — env
	// names are the operator's own (the platform never hardcodes "prod"):
	// `promotion: {from: stage}` makes `idpctl promote` refuse any other
	// source env, so a digest can only reach this env through its gate.
	Promotion *PromotionConfig `json:"promotion,omitempty"`

	// Seams declares which platform capabilities this env PROVIDES. It turns the
	// loose coupling into a fail-closed contract: an app may only request a seam
	// the env provides (policy), and an env may only claim a seam it actually
	// backs (Validate). Omit it and the seams derive permissively from the rest
	// of this config (back-compat). See Seams / EffectiveSeams.
	Seams *Seams `json:"seams,omitempty"`

	// Modules is the platform module registry for this env.
	Modules map[string]Module `json:"modules,omitempty"`
}

// Seams declares the platform capabilities ("seams" — the interfaces apps
// depend on: data stores, LAN exposure, public routes, autoscaling, volumes)
// that an environment provides. Each is a *bool: nil = DERIVE the default from
// the rest of cluster.yaml (so existing envs keep working); explicit true/false
// overrides. Resolve to concrete values with EffectiveSeams.
type Seams struct {
	// StatefulStores: the env hosts in-cluster db/cache (the dev-postgres /
	// dev-redis path). false => apps must NOT declare db/cache here and instead
	// receive DATABASE_URL/REDIS_URL from the secrets backend (managed/external
	// — the prod posture; keeps prod PVC-free). Default: true.
	StatefulStores *bool `json:"statefulStores,omitempty"`
	// LANExpose: MetalLB is present, so apps may use expose.lan. false => no LAN
	// LoadBalancers (prod is Cloudflare-Tunnel-only). Default: true.
	LANExpose *bool `json:"lanExpose,omitempty"`
	// PublicRoutes: public routes[] are allowed (an ingress/tunnel path exists).
	// Default: derived (true iff zones are declared).
	PublicRoutes *bool `json:"publicRoutes,omitempty"`
	// Autoscale: sizing.autoscale is available. Default: derived (true iff the
	// keda module is enabled).
	Autoscale *bool `json:"autoscale,omitempty"`
	// Volumes: volumes[] (nfs/pvc/emptyDir) are mountable. Default: true.
	Volumes *bool `json:"volumes,omitempty"`
}

// ResolvedSeams is the concrete seam set after defaults/derivation.
type ResolvedSeams struct {
	StatefulStores, LANExpose, PublicRoutes, Autoscale, Volumes bool
}

// EffectiveSeams resolves the declared Seams against derivation defaults so
// callers (policy, doctor) get concrete booleans. Derivation: PublicRoutes
// follows whether zones are declared; Autoscale follows the keda module;
// StatefulStores / LANExpose / Volumes default true (the historical behavior —
// envs opt OUT to tighten, e.g. prod).
func (c *Config) EffectiveSeams() ResolvedSeams {
	kedaOn := false
	if m, ok := c.Modules["keda"]; ok && m.Enabled {
		kedaOn = true
	}
	pick := func(p *bool, def bool) bool {
		if p != nil {
			return *p
		}
		return def
	}
	s := c.Seams
	if s == nil {
		s = &Seams{}
	}
	return ResolvedSeams{
		StatefulStores: pick(s.StatefulStores, true),
		LANExpose:      pick(s.LANExpose, true),
		PublicRoutes:   pick(s.PublicRoutes, len(c.Zones) > 0),
		Autoscale:      pick(s.Autoscale, kedaOn),
		Volumes:        pick(s.Volumes, true),
	}
}

// PromotionConfig declares the env's promotion gate (see Config.Promotion).
type PromotionConfig struct {
	// From is the only env `idpctl promote` accepts as a source for this env
	// (e.g. prod declares from: stage). Empty = any source allowed.
	From string `json:"from,omitempty"`
}

// SecretsConfig is the env's secrets backend + store reference.
type SecretsConfig struct {
	// Backend is local|ssm.
	Backend string `json:"backend,omitempty"`

	// StoreRef points the app chart's ExternalSecret at the env's
	// SecretStore/ClusterSecretStore.
	StoreRef StoreRef `json:"storeRef,omitempty"`

	// RefreshInterval is how often external-secrets re-reads the backend.
	RefreshInterval string `json:"refreshInterval,omitempty"`

	// ImagePull configures the registry pull-secret the app chart materializes
	// into EVERY app namespace via its own ExternalSecret — replacing any
	// cluster-side ClusterExternalSecret with a hand-maintained namespace list.
	// Unset => the chart renders no pull-secret ES (public images, or the
	// secret is provided out-of-band).
	ImagePull *ImagePullConfig `json:"imagePull,omitempty"`
}

// ImagePullConfig is the per-env registry pull-secret wiring.
type ImagePullConfig struct {
	// SecretName is the Secret the pods reference in imagePullSecrets and the
	// ExternalSecret's target. Defaults to "ghcr-pull".
	SecretName string `json:"secretName,omitempty"`

	// RemoteKey is the backend key holding the registry dockerconfigjson
	// (e.g. /homelab/ghcr/pull-dockerconfigjson). Required when ImagePull set.
	RemoteKey string `json:"remoteKey,omitempty"`

	// StoreRef is the store the pull ES reads from. It often differs from the
	// env's main secrets store (e.g. dev runtime secrets use the local
	// Kubernetes provider while the registry credential lives in SSM).
	// Defaults to the env's secrets.storeRef.
	StoreRef StoreRef `json:"storeRef,omitempty"`
}

// StoreRef references an external-secrets SecretStore or ClusterSecretStore.
type StoreRef struct {
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// Observability holds the platform endpoints injected as Tier-A env.
type Observability struct {
	LokiURL        string `json:"lokiURL,omitempty"`
	OTLPEndpoint   string `json:"otlpEndpoint,omitempty"`
	ConsoleLogging *bool  `json:"consoleLogging,omitempty"`
}

// ResourceBounds caps requests/limits. Values are Kubernetes quantity strings.
type ResourceBounds struct {
	MaxCPU    string `json:"maxCPU,omitempty"`    // e.g. "2"
	MaxMemory string `json:"maxMemory,omitempty"` // e.g. "2Gi"
}

// FluxConfig is the env's Flux wiring.
type FluxConfig struct {
	// Namespace is the namespace Flux runs in and where the shared GitRepository
	// source + every HelmRelease live (the Flux bootstrap creates the source
	// named SourceName here). Defaults to "flux-system".
	Namespace string `json:"namespace,omitempty"`

	// RepoURL is the git repo the FluxInstance syncs from (this platform repo).
	// REQUIRED — instance identity, never defaulted (see idp.yaml / Validate).
	RepoURL string `json:"repoURL,omitempty"`

	// Branch is the git branch the FluxInstance tracks; the GitRepository ref is
	// refs/heads/<branch>. Defaults to "main".
	Branch string `json:"branch,omitempty"`

	// SourceName is the name of the GitRepository source the Flux bootstrap
	// provides (the FluxInstance.sync creates one); HelmReleases reference it
	// cross-namespace as {kind: GitRepository, name: SourceName, namespace:
	// Namespace}. Defaults to "flux-system".
	SourceName string `json:"sourceName,omitempty"`
}

// Module is one entry in the registry: a Helm release Flux reconciles.
type Module struct {
	Enabled   bool           `json:"enabled,omitempty"`
	Source    string         `json:"source,omitempty"` // localChart | chartRepo
	Chart     string         `json:"chart,omitempty"`  // charts/infra/<x> OR repo chart name
	RepoURL   string         `json:"repoURL,omitempty"`
	Version   string         `json:"version,omitempty"` // required for chartRepo
	Namespace string         `json:"namespace,omitempty"`
	Values    map[string]any `json:"values,omitempty"`

	// DisableWait makes the module's Helm install/upgrade NOT wait for its
	// resources to become ready. Needed when a module ships a resource that
	// cannot be ready until it is consumed — e.g. a PVC on a
	// WaitForFirstConsumer StorageClass (image-builder's BuildKit cache binds
	// on the first build Job, so a readiness wait deadlocks the install).
	DisableWait bool `json:"disableWait,omitempty"`
}

// Defaults that apply when cluster.yaml omits a field.
const (
	DefaultDomain = "svc.cluster.local"
	// DefaultFluxNamespace is where Flux runs and where the shared GitRepository
	// source + every HelmRelease live.
	DefaultFluxNamespace = "flux-system"
	// DefaultFluxSourceName is the name of the GitRepository source the Flux
	// bootstrap (FluxInstance.sync) provides; HelmReleases reference it
	// cross-namespace.
	DefaultFluxSourceName = "flux-system"
	// DefaultBranch is the git branch the FluxInstance tracks (ref
	// refs/heads/<branch>) when cluster.yaml omits flux.branch.
	DefaultBranch  = "main"
	DefaultRefresh = "1h"
)

// BranchRef returns the GitRepository ref for a branch: refs/heads/<branch>.
func BranchRef(branch string) string {
	if branch == "" {
		branch = DefaultBranch
	}
	return "refs/heads/" + branch
}

// Load reads and parses environments/<env>/cluster.yaml, applies defaults, and
// validates it. The env name defaults from the path if the file omits it.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cluster config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse cluster config %q: %w", path, err)
	}
	if c.Env == "" {
		c.Env = envFromPath(path)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid cluster config %q: %w", path, err)
	}
	return &c, nil
}

// LoadForEnv loads environments/<env>/cluster.yaml relative to a platform repo
// root.
func LoadForEnv(root, env string) (*Config, error) {
	return Load(filepath.Join(root, "environments", env, "cluster.yaml"))
}

// envFromPath extracts <env> from .../environments/<env>/cluster.yaml.
func envFromPath(path string) string {
	dir := filepath.Dir(path)    // .../environments/<env>
	parent := filepath.Base(dir) // <env>
	if filepath.Base(filepath.Dir(dir)) == "environments" {
		return parent
	}
	return parent
}

// ApplyDefaults fills env-conventional defaults in place (idempotent).
func (c *Config) ApplyDefaults() {
	if c.Domain == "" {
		c.Domain = DefaultDomain
	}
	if c.Flux.Namespace == "" {
		c.Flux.Namespace = DefaultFluxNamespace
	}
	if c.Flux.SourceName == "" {
		c.Flux.SourceName = DefaultFluxSourceName
	}
	if c.Flux.Branch == "" {
		c.Flux.Branch = DefaultBranch
	}
	// Flux.RepoURL is deliberately NOT defaulted: it is instance identity (which
	// platform repo this cluster syncs from). Validate rejects an empty value so
	// a fork can never silently sync from someone else's repo.
	if c.Secrets.RefreshInterval == "" {
		c.Secrets.RefreshInterval = DefaultRefresh
	}
	// Backend default by env name when unset.
	if c.Secrets.Backend == "" {
		if c.Env == "prod" {
			c.Secrets.Backend = BackendSSM
		} else {
			c.Secrets.Backend = BackendLocal
		}
	}
	if c.Secrets.StoreRef.Kind == "" {
		c.Secrets.StoreRef.Kind = KindClusterSecretStore
	}
	if ip := c.Secrets.ImagePull; ip != nil {
		if ip.SecretName == "" {
			ip.SecretName = "ghcr-pull"
		}
		if ip.StoreRef.Name == "" {
			ip.StoreRef = c.Secrets.StoreRef
		}
		if ip.StoreRef.Kind == "" {
			ip.StoreRef.Kind = KindClusterSecretStore
		}
	}
}

// Validate checks the env config is internally consistent.
func (c *Config) Validate() error {
	if c.Flux.RepoURL == "" {
		return fmt.Errorf("flux.repoURL is required: the git URL of YOUR platform repo (this is instance identity — no default is provided so a fork cannot silently sync from someone else's repo)")
	}
	if c.Secrets.Backend != BackendLocal && c.Secrets.Backend != BackendSSM {
		return fmt.Errorf("secrets.backend %q must be %q or %q", c.Secrets.Backend, BackendLocal, BackendSSM)
	}
	if c.Secrets.StoreRef.Name == "" {
		return fmt.Errorf("secrets.storeRef.name is required")
	}
	if k := c.Secrets.StoreRef.Kind; k != KindClusterSecretStore && k != KindSecretStore {
		return fmt.Errorf("secrets.storeRef.kind %q must be %q or %q", k, KindClusterSecretStore, KindSecretStore)
	}
	if ip := c.Secrets.ImagePull; ip != nil && ip.RemoteKey == "" {
		return fmt.Errorf("secrets.imagePull.remoteKey is required when imagePull is set (the backend key holding the registry dockerconfigjson)")
	}
	for name, m := range c.Modules {
		if !m.Enabled {
			continue
		}
		switch m.Source {
		case SourceLocalChart:
			if m.Chart == "" {
				return fmt.Errorf("module %q: chart path required for localChart", name)
			}
		case SourceChartRepo:
			if m.Chart == "" {
				return fmt.Errorf("module %q: chart name required for chartRepo", name)
			}
			if m.Version == "" {
				return fmt.Errorf("module %q: version is required for chartRepo (must be pinned)", name)
			}
		default:
			return fmt.Errorf("module %q: source %q must be %q or %q", name, m.Source, SourceLocalChart, SourceChartRepo)
		}
	}

	// Seam coherence: an env may only DECLARE a seam it actually backs. (The
	// resolver derives sensible defaults; these checks catch a declaration that
	// contradicts the rest of cluster.yaml — required stores/routes/endpoints.)
	seams := c.EffectiveSeams()
	if seams.PublicRoutes && len(c.Zones) == 0 {
		return fmt.Errorf("seams.publicRoutes is on but no zones are declared — a public route would have no approved host zone to validate against")
	}
	if seams.Autoscale {
		if m, ok := c.Modules["keda"]; !ok || !m.Enabled {
			return fmt.Errorf("seams.autoscale is on but the keda module is not enabled — apps could request autoscaling with nothing to drive it")
		}
	}
	// Logs are a UNIVERSAL seam — every app logs, so lokiURL is required. OTLP
	// (traces/metrics) is OPTIONAL: not every cluster runs a collector. Declare
	// otlpEndpoint only if the env actually provides one (it's injected as
	// OTEL_EXPORTER_OTLP_ENDPOINT only when set — see render.TierAEnv).
	if c.Observability.LokiURL == "" {
		return fmt.Errorf("observability.lokiURL is required (injected into every app as Tier-A env) — declare the log endpoint this env provides")
	}
	return nil
}

// Per-app dev Postgres defaults. The shared dev-postgres MODULE is gone: in dev,
// when an app declares db: postgres, the render path emits a DEDICATED
// dev-postgres Flux HelmRelease per app, with targetNamespace
// <app>-<env>-postgres. The Helm release name is the per-app store name
// (<app>-postgres); the dev-postgres chart's fullname helper collapses that
// release to the Service name, so the in-cluster Service DNS is
// <app>-postgres.<app>-<env>-postgres.svc.cluster.local.
//
// These constants are the dev "data store defaults" (DB name, placeholder
// password, node pin) that used to live in environments/dev/cluster.yaml's
// dev-postgres module values. They now live here so the renderer and the
// per-app Postgres HelmRelease stay in lockstep.
const (
	// DevPostgresTool is the store "tool"/purpose segment of the per-app Postgres
	// namespace and the suffix of its Flux HelmRelease / Helm release name.
	DevPostgresTool        = "postgres"
	DevPostgresDefaultPort = "5432"
	DevPostgresDefaultUser = "app"
	// DevPostgresDefaultPassword is a clearly-marked DEV placeholder. The renderer
	// only emits it for the local (dev) backend so an app can boot against its
	// per-app in-cluster dev Postgres with no external-secrets operator. It is
	// pinned identically into the rendered dev-postgres HelmRelease
	// (values.auth.password) so the rendered DATABASE_URL matches what the chart
	// provisions. NEVER a real secret; prod uses backend=ssm and never sees this.
	DevPostgresDefaultPassword = "dev-postgres-placeholder"
	// DevPostgresNode optionally pins the per-app dev Postgres to a baseline node so
	// the local-path PVC always reschedules onto the box that holds the data. Empty
	// = no pin (the scheduler picks any node that can pull + run the image). Left
	// empty for the homelab because its bare-metal node ("optiplex") can't reach
	// external registries; set this to a real node hostname to pin in other envs.
	DevPostgresNode = ""
)

// DevPostgresReleaseName is the Helm release name for an app's dedicated dev
// Postgres HelmRelease: <app>-postgres.
func DevPostgresReleaseName(app string) string {
	return app + "-" + DevPostgresTool
}

// DevPostgresService is the in-cluster Service name for an app's dedicated dev
// Postgres. The dev-postgres chart fullname helper collapses to the release name
// when the chart name ("dev-postgres") is already contained in the release;
// <app>-postgres does NOT contain "dev-postgres", so the fullname is
// <release>-<chartname> = <app>-postgres-dev-postgres.
func DevPostgresService(app string) string {
	return DevPostgresReleaseName(app) + "-dev-postgres"
}

// DevPostgresNamespace is the dedicated namespace for an app's dev Postgres:
// <app>-<env>-postgres.
func DevPostgresNamespace(app, env string) string {
	return appDNSLabel(app + "-" + env + "-" + DevPostgresTool)
}

// DevPostgresDatabase is the per-app database name on the app's dedicated dev
// Postgres. It is derived from the app name so each app gets its own DB.
func DevPostgresDatabase(app string) string {
	return appDNSLabel(app)
}

// ── dev Redis (mirrors dev Postgres) ─────────────────────────────────────────
const (
	// DevRedisTool is the store tool/purpose segment of the per-app Redis namespace
	// and the suffix of its Flux/Helm release name.
	DevRedisTool        = "redis"
	DevRedisDefaultPort = "6379"
)

// DevRedisReleaseName is the Helm release name for an app's dedicated dev Redis:
// <app>-redis.
func DevRedisReleaseName(app string) string { return app + "-" + DevRedisTool }

// DevRedisService is the in-cluster Service name for an app's dev Redis. Like the
// Postgres helper, the dev-redis chart fullname collapses to <release>-<chartname>
// (<app>-redis does NOT contain "dev-redis"): <app>-redis-dev-redis.
func DevRedisService(app string) string { return DevRedisReleaseName(app) + "-dev-redis" }

// DevRedisNamespace is the dedicated namespace for an app's dev Redis:
// <app>-<env>-redis.
func DevRedisNamespace(app, env string) string {
	return appDNSLabel(app + "-" + env + "-" + DevRedisTool)
}

// DevRedisURL returns a working in-cluster Redis URL for the dev (local) backend,
// pointing at the app's dedicated per-app dev Redis. name is the requested cache's
// handle (informational). Returns "" when there is no env config. No auth/db (the
// dev Redis is an in-cluster, no-persistence pub/sub bus).
func DevRedisURL(c *Config, app, name string) string {
	if c == nil {
		return ""
	}
	host := fmt.Sprintf("%s.%s.%s", DevRedisService(app), DevRedisNamespace(app, c.Env), c.Domain)
	return fmt.Sprintf("redis://%s:%s", host, DevRedisDefaultPort)
}

// DevDatabaseURL returns a working in-cluster Postgres connection URL for the dev
// (local) backend, pointing at the app's DEDICATED per-app dev Postgres. app is
// the owning app name; dbName is the requested db's logical handle (informational
// — the per-app dev Postgres serves a single database named after the app).
// Returns "" when there is no env config.
//
// The URL host is the CROSS-NAMESPACE FQDN
// <pg-service>.<app>-<env>-postgres.svc.cluster.local:<port>/<db> with
// sslmode=disable (in-cluster dev traffic). User/password/db come from the dev
// data-store defaults above; password is the marked dev placeholder.
func DevDatabaseURL(c *Config, app, dbName string) string {
	if c == nil {
		return ""
	}
	ns := DevPostgresNamespace(app, c.Env)
	svc := DevPostgresService(app)
	user := DevPostgresDefaultUser
	pass := DevPostgresDefaultPassword
	db := DevPostgresDatabase(app)
	port := DevPostgresDefaultPort
	// c.Domain is the cluster-internal DNS domain (defaults to svc.cluster.local),
	// so the FQDN is <service>.<ns>.<domain> — no extra "svc." literal.
	host := fmt.Sprintf("%s.%s.%s", svc, ns, c.Domain)
	return fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?sslmode=disable", user, pass, host, port, db)
}

// appDNSLabel lowercases s and reduces it to a valid RFC1123 DNS label (only
// [a-z0-9-], no leading/trailing/double dashes, max 63 chars). It mirrors
// appconfig.SanitizeDNSLabel; it is duplicated here (a few lines) only to keep
// clusterenv dependency-free of appconfig (appconfig must not import clusterenv,
// and clusterenv is imported by render alongside appconfig).
func appDNSLabel(s string) string {
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

// ConsoleLoggingValue returns the effective CONSOLE_LOGGING string ("true" by
// default), suitable for direct injection as a Tier-A env value.
func (o Observability) ConsoleLoggingValue() string {
	if o.ConsoleLogging != nil && !*o.ConsoleLogging {
		return "false"
	}
	return "true"
}

// HostInZone reports whether host falls under any approved zone. A zone matches
// if the host equals it or ends with "."+zone. An empty zone list means no
// zones are configured (policy treats that as "unrestricted" for dev usability;
// prod configs should always set zones).
func (c *Config) HostInZone(host string) bool {
	if len(c.Zones) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, z := range c.Zones {
		z = strings.TrimSuffix(strings.ToLower(z), ".")
		// Wildcard zone "*.example.com" matches any subdomain of example.com
		// (and the apex example.com itself).
		if strings.HasPrefix(z, "*.") {
			base := z[2:]
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		// Exact zone "carshowdb.local" matches that host or any subdomain.
		if host == z || strings.HasSuffix(host, "."+z) {
			return true
		}
	}
	return false
}
