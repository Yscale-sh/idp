package render

import (
	"fmt"
	"strings"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/clusterenv"
	"github.com/jakenesler/jdp/internal/policy"
)

// Values is the chart values document, matching charts/app/values.yaml keys
// exactly. It is a typed shape (not a free map) so the renderer cannot silently
// drift from the chart contract; it serializes to the same YAML the chart
// consumes. JSON tags drive sigs.k8s.io/yaml output (sorted, deterministic).
type Values struct {
	Image            ImageValues             `json:"image"`
	ImagePullSecrets []ImagePullSecretValues `json:"imagePullSecrets"`
	Port             int                     `json:"port"`
	Replicas         int                     `json:"replicas"`
	Resources        ResourceRequirements    `json:"resources"`
	Service          ServiceValues           `json:"service"`
	Probes           ProbesValues            `json:"probes"`
	Routes           []RouteValues           `json:"routes"`
	Tunnel           *TunnelValues           `json:"tunnel,omitempty"`
	Env              EnvValues               `json:"env"`
	ExternalSecret   ExternalSecretValues    `json:"externalSecret"`
	Keda             KedaValues              `json:"keda"`
	ServiceMonitor   ServiceMonitorValues    `json:"serviceMonitor"`
	Pdb              PdbValues               `json:"pdb"`
	DB               []StoreValues           `json:"db"`
	Cache            []StoreValues           `json:"cache"`
	// DevSecretPlaceholders are clearly-marked dev placeholder values for
	// app-level secret keys (JWT_SECRET, GEMINI_*, S3_*, SENDGRID_*, STRIPE_*).
	// They are emitted ONLY for the local (dev) backend so an app boots with no
	// external-secrets operator; the chart writes them into the plain runtime
	// Secret. Empty for the ssm/prod backend (SSM supplies the real values).
	DevSecretPlaceholders []SecretPlaceholderValues `json:"devSecretPlaceholders"`
	Platform              PlatformValues            `json:"platform"`
}

// SecretPlaceholderValues is one dev-placeholder runtime-Secret entry: a key plus
// a clearly-marked dev value. NEVER a real secret.
type SecretPlaceholderValues struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ImageValues struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	PullPolicy string `json:"pullPolicy"`
}

// ImagePullSecretValues is one entry in the pod imagePullSecrets list. Kubernetes
// requires the {name: <secret>} object shape (NOT a bare string), so the renderer
// emits this typed form to round-trip cleanly into the pod spec.
type ImagePullSecretValues struct {
	Name string `json:"name"`
}

type ResourceSpec struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type ResourceRequirements struct {
	Requests ResourceSpec `json:"requests"`
	Limits   ResourceSpec `json:"limits"`
}

type ServiceValues struct {
	Port int `json:"port"`
	// NOTE: there is NO Type field, by design. ClusterIP is hardcoded in the
	// chart template. This is the LoadBalancer guardrail at the values layer.
}

type ProbeValues struct {
	Path                string `json:"path"`
	InitialDelaySeconds int    `json:"initialDelaySeconds"`
	PeriodSeconds       int    `json:"periodSeconds"`
}

type ProbesValues struct {
	Liveness  ProbeValues `json:"liveness"`
	Readiness ProbeValues `json:"readiness"`
}

type RouteAccessValues struct {
	Humans       bool `json:"humans"`
	ServiceToken bool `json:"serviceToken"`
}

type RouteValues struct {
	Host   string            `json:"host"`
	Public bool              `json:"public"`
	Access RouteAccessValues `json:"access"`
}

// TunnelValues renders the cloudflared (Cloudflare Tunnel) sidecar config. The
// renderer sets it ONLY for a non-local backend (prod) app that declares a public
// route; the chart gates the cloudflared sidecar + ingress ConfigMap on
// tunnel.enabled. The Values field is a pointer with omitempty, so it is ABSENT
// (render byte-identical to today) for every app that doesn't opt in — i.e. all
// of dev and any prod app with no public route. The tunnel IS the ingress, so a
// public route is served with NO LoadBalancer (the no-LB policy holds).
type TunnelValues struct {
	Enabled bool                  `json:"enabled"`
	Image   string                `json:"image,omitempty"`
	Ingress []TunnelIngressValues `json:"ingress"`
}

// TunnelIngressValues is one cloudflared ingress rule: a public hostname served
// through the tunnel to the app's OWN Service on localhost:<port> inside the pod.
type TunnelIngressValues struct {
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
}

type EnvValues struct {
	TierA map[string]string `json:"tierA"`
	Extra map[string]string `json:"extra"`
}

type StoreRefValues struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

type RemoteRefValues struct {
	SecretKey string            `json:"secretKey"`
	RemoteRef map[string]string `json:"remoteRef"`
}

type DataFromValues struct {
	Extract map[string]string `json:"extract"`
}

type ExternalSecretValues struct {
	Enabled         bool              `json:"enabled"`
	Backend         string            `json:"backend"`
	RefreshInterval string            `json:"refreshInterval"`
	StoreRef        StoreRefValues    `json:"storeRef"`
	DataFrom        []DataFromValues  `json:"dataFrom"`
	RemoteRefs      []RemoteRefValues `json:"remoteRefs"`
}

type KedaHTTPValues struct {
	Hosts                 []string `json:"hosts"`
	PathPrefixes          []string `json:"pathPrefixes"`
	TargetPendingRequests int      `json:"targetPendingRequests"`
	ScaledownPeriod       int      `json:"scaledownPeriod"`
}

type KedaValues struct {
	Enabled     bool             `json:"enabled"`
	Kind        string           `json:"kind"`
	MinReplicas int              `json:"minReplicas"`
	MaxReplicas int              `json:"maxReplicas"`
	Triggers    []map[string]any `json:"triggers"`
	HTTP        KedaHTTPValues   `json:"http"`
}

type ServiceMonitorValues struct {
	Enabled      bool   `json:"enabled"`
	Path         string `json:"path"`
	Port         string `json:"port"`
	Interval     string `json:"interval"`
	ReleaseLabel string `json:"releaseLabel"`
}

type PdbValues struct {
	Enabled      bool `json:"enabled"`
	MinAvailable int  `json:"minAvailable"`
}

// StoreValues is one data-store (db/cache) wiring hint the chart consumes to
// emit the *_URL env vars (Tier D). The first/default entry of each class also
// gets the bare DATABASE_URL / REDIS_URL alias; every entry gets the prefixed
// <NAME>_DATABASE_URL / <NAME>_REDIS_URL form.
//
// UrlKeys are the env-var names this store contributes (e.g. ["DATABASE_URL",
// "PRIMARY_DATABASE_URL"]). The chart wires each of these to the same connection.
//
// Connection carries the resolved coordinates for the dev (local) backend, where
// the chart assembles a working connection URL itself (the in-cluster dev store).
// For the ssm/prod backend the URL is materialized from the secrets backend
// instead, so Connection.URL is empty and the chart wires the keys from the
// runtime Secret. Connection is never a real secret in dev — it points at the
// in-cluster dev store with a clearly-marked dev password.
type StoreValues struct {
	Name       string           `json:"name"`
	Type       string           `json:"type"`
	Default    bool             `json:"default"`
	UrlKeys    []string         `json:"urlKeys"`
	Connection *StoreConnection `json:"connection,omitempty"`
}

// StoreConnection is the resolved connection for the dev (local) backend. When
// set, the chart writes the assembled URL into the runtime Secret under each of
// the store's UrlKeys. host/port/database/user/password describe the in-cluster
// dev store; URL is the assembled connection string.
type StoreConnection struct {
	URL string `json:"url"`
}

type PlatformValues struct {
	App       string `json:"app"`
	Env       string `json:"env"`
	Product   string `json:"product,omitempty"`
	Component string `json:"component,omitempty"`
	ManagedBy string `json:"managedBy"`
}

// ReleaseLabel default for ServiceMonitor discovery by kube-prometheus-stack.
const DefaultReleaseLabel = "prometheus"

// Default KEDA ScaledObject trigger when autoscale is enabled without an explicit
// metric/triggers. A CPU-utilization trigger is the safe, universally-available
// default and guarantees the chart always renders a valid (non-empty) ScaledObject.
const (
	DefaultAutoscaleMetric = "cpu"
	DefaultAutoscaleTarget = "70"
)

// BuildValues renders chart values from the defaults-applied app + env config.
// image is the fully-qualified CI image (repo:tag); deployTime is the CI stamp.
func BuildValues(app appconfig.App, env string, c *clusterenv.Config, image, deployTime string) (Values, error) {
	repo, tag := splitImage(image, app.Runtime.Image)

	profile := app.Sizing.Profile
	if profile == "" {
		profile = appconfig.DefaultProfile
	}
	envelope, ok := policy.ProfileResources(profile)
	if !ok {
		return Values{}, fmt.Errorf("unknown resource profile %q", profile)
	}

	var obs clusterenv.Observability
	if c != nil {
		obs = c.Observability
	}

	v := Values{
		Image: ImageValues{
			Repository: repo,
			Tag:        tag,
			PullPolicy: "IfNotPresent",
		},
		ImagePullSecrets: defaultImagePullSecrets(),
		Port:             app.Runtime.Port,
		Replicas:         app.Sizing.Replicas,
		Resources: ResourceRequirements{
			Requests: ResourceSpec{CPU: envelope.Requests.CPU, Memory: envelope.Requests.Memory},
			Limits:   ResourceSpec{CPU: envelope.Limits.CPU, Memory: envelope.Limits.Memory},
		},
		Service: ServiceValues{Port: app.Runtime.Port},
		Probes: ProbesValues{
			Liveness:  ProbeValues{Path: "/healthz", InitialDelaySeconds: 10, PeriodSeconds: 10},
			Readiness: ProbeValues{Path: "/readyz", InitialDelaySeconds: 5, PeriodSeconds: 10},
		},
		Routes: buildRoutes(app),
		Tunnel: buildTunnel(app, c),
		Env: EnvValues{
			TierA: TierAEnv(app, env, obs, deployTime),
			Extra: map[string]string{},
		},
		ExternalSecret:        BuildExternalSecret(app, env, c),
		Keda:                  buildKeda(app),
		ServiceMonitor:        buildServiceMonitor(app),
		Pdb:                   buildPDB(app),
		DB:                    buildStores(app, app.DB, "DATABASE_URL", env, c),
		Cache:                 buildStores(app, app.Cache, "REDIS_URL", env, c),
		DevSecretPlaceholders: buildDevPlaceholders(app, c),
		Platform: PlatformValues{
			App:       app.App,
			Env:       env,
			Product:   app.Product,
			Component: app.Component,
			ManagedBy: "platformctl",
		},
	}

	// connectsTo resolves into env.extra (non-secret addresses).
	conns, err := ResolveConnections(app, env, c)
	if err != nil {
		return Values{}, err
	}
	for _, conn := range conns {
		v.Env.Extra[conn.EnvVar] = conn.Value
	}

	return v, nil
}

func buildRoutes(app appconfig.App) []RouteValues {
	out := make([]RouteValues, 0, len(app.Routes))
	for _, r := range app.Routes {
		out = append(out, RouteValues{
			Host:   r.Host,
			Public: r.Public,
			Access: RouteAccessValues{Humans: r.Access.Humans, ServiceToken: r.Access.ServiceToken},
		})
	}
	return out
}

// hasPublicRoute reports whether the app declares any public route (public: true
// with a host). Public exposure is what wires a Cloudflare Tunnel.
func hasPublicRoute(app appconfig.App) bool {
	for _, r := range app.Routes {
		if r.Public && r.Host != "" {
			return true
		}
	}
	return false
}

// buildTunnel returns the cloudflared (Cloudflare Tunnel) config for an app's
// public routes, or nil when the tunnel does not apply (so the values field is
// omitted and the render stays byte-identical). It applies ONLY for a non-local
// backend (prod) app with a public route: dev keeps its in-cluster .local routes
// and never tunnels. Each public host maps through the tunnel to the app's OWN
// Service on localhost:<port> in the pod — exposure with NO LoadBalancer.
func buildTunnel(app appconfig.App, c *clusterenv.Config) *TunnelValues {
	if isLocalBackend(c) || !hasPublicRoute(app) {
		return nil
	}
	svc := fmt.Sprintf("http://localhost:%d", app.Runtime.Port)
	ingress := make([]TunnelIngressValues, 0)
	for _, r := range app.Routes {
		if r.Public && r.Host != "" {
			ingress = append(ingress, TunnelIngressValues{Hostname: r.Host, Service: svc})
		}
	}
	return &TunnelValues{Enabled: true, Ingress: ingress}
}

// DefaultImagePullSecret is the registry-credentials Secret every app pulls its
// ghcr.io image through. Kubernetes wants the {name: <secret>} object shape.
const DefaultImagePullSecret = "ghcr-pull"

func defaultImagePullSecrets() []ImagePullSecretValues {
	return []ImagePullSecretValues{{Name: DefaultImagePullSecret}}
}

// buildStores builds the chart's db/cache wiring hints. suffix is "DATABASE_URL"
// for db and "REDIS_URL" for cache. Each store contributes its env-var keys (the
// first/default entry also gets the bare alias). For the dev (local) backend the
// renderer resolves a concrete in-cluster connection URL so the chart's plain
// runtime Secret can carry a WORKING URL with no external-secrets operator; for
// the ssm/prod backend the URL is left to the secrets backend (Connection nil).
func buildStores(app appconfig.App, stores []appconfig.DataStore, suffix, env string, c *clusterenv.Config) []StoreValues {
	out := make([]StoreValues, 0, len(stores))
	for i, s := range stores {
		sv := StoreValues{
			Name:    s.Name,
			Type:    s.Type,
			Default: i == 0,
			UrlKeys: storeKeys([]appconfig.DataStore{s}, suffix, i == 0),
		}
		// Only Postgres wires a dev connection URL today (Redis dev wiring lands
		// when a dev-redis chart exists). For the local backend, resolve the
		// CROSS-NAMESPACE URL pointing at the app's DEDICATED per-app dev Postgres
		// (rendered as a sibling Application into <app>-<env>-postgres) so the app
		// boots against its own in-cluster store.
		if local := isLocalBackend(c); local && s.Type == appconfig.DefaultDBType {
			if url := clusterenv.DevDatabaseURL(c, app.App, s.Name); url != "" {
				sv.Connection = &StoreConnection{URL: url}
			}
		}
		out = append(out, sv)
	}
	return out
}

// isLocalBackend reports whether the env's secrets backend is "local" (dev/
// on-prem), which uses a plain in-cluster Secret rather than external-secrets.
func isLocalBackend(c *clusterenv.Config) bool {
	if c == nil {
		return true // no env config => dev-style local default
	}
	return c.Secrets.Backend != clusterenv.BackendSSM
}

// DevPlaceholderValue is the literal stamped into every dev placeholder secret
// key. It is intentionally obvious so nobody mistakes it for a real credential.
const DevPlaceholderValue = "dev-placeholder"

// devAppSecretKeys is the app-level secret env every onboarded app needs to BOOT
// in dev (independent of declared capabilities). These get a clearly-marked dev
// placeholder so the app starts with no external-secrets operator; prod supplies
// the real values from SSM. Ordered for deterministic render output.
var devAppSecretKeys = []string{
	"JWT_SECRET",
	"GEMINI_API_KEY",
	"GEMINI_MODEL",
	"SENDGRID_API_KEY",
	"STRIPE_SECRET_KEY",
	"STRIPE_PUBLISHABLE_KEY",
	"STRIPE_WEBHOOK_SECRET",
}

// buildDevPlaceholders returns the dev placeholder secret entries for the local
// backend (empty for ssm/prod). It covers the universal app-level keys plus the
// S3/object-storage keys derived from each declared storage bucket, so a typical
// app (e.g. carshowdb) boots in dev. Every value is the marked dev placeholder —
// never a real secret. Keys collide-free with the *_URL keys (those come from the
// data-store connection, not from here).
func buildDevPlaceholders(app appconfig.App, c *clusterenv.Config) []SecretPlaceholderValues {
	if !isLocalBackend(c) {
		return []SecretPlaceholderValues{}
	}
	keys := make([]string, 0, len(devAppSecretKeys)+len(app.Storage)*4)
	keys = append(keys, devAppSecretKeys...)
	// Object-storage credentials per declared bucket (Tier C storage convention).
	keys = append(keys, StorageEnvKeys(app)...)

	out := make([]SecretPlaceholderValues, 0, len(keys))
	for _, k := range keys {
		out = append(out, SecretPlaceholderValues{Name: k, Value: DevPlaceholderValue})
	}
	return out
}

func buildKeda(app appconfig.App) KedaValues {
	as := app.Sizing.Autoscale
	k := KedaValues{
		Enabled:     as.Enabled,
		Kind:        appconfig.DefaultAutoscaleK,
		MinReplicas: app.Sizing.Replicas,
		MaxReplicas: app.Sizing.Replicas,
		Triggers:    []map[string]any{},
		HTTP: KedaHTTPValues{
			Hosts:                 hostsOf(app),
			PathPrefixes:          []string{"/"},
			TargetPendingRequests: 100,
			ScaledownPeriod:       300,
		},
	}
	if !as.Enabled {
		return k
	}
	if as.Kind != "" {
		k.Kind = as.Kind
	}
	if as.ScaleToZero {
		// scale-to-zero is sugar for HTTPScaledObject + min 0.
		k.Kind = appconfig.HTTPScaledObjectK
	}
	if as.Min > 0 || k.Kind == appconfig.HTTPScaledObjectK {
		k.MinReplicas = as.Min
	}
	if as.ScaleToZero {
		k.MinReplicas = 0 // authoritative: idle apps go fully down
	}
	if as.Max > 0 {
		k.MaxReplicas = as.Max
	}
	// Triggers (ScaledObject only — HTTPScaledObject carries its own scaling
	// metric, not a triggers list). Precedence:
	//   1. explicit raw Triggers passthrough,
	//   2. convenience Metric/Target pair,
	//   3. a sane default CPU-utilization trigger.
	// The chart REQUIRES at least one trigger for a ScaledObject, so the renderer
	// must never emit keda.enabled with an empty triggers list. Defaulting here
	// keeps the "just enable autoscale" case (min/max only, no metric) working
	// end-to-end instead of failing at `helm template`.
	if k.Kind == appconfig.DefaultAutoscaleK {
		switch {
		case len(as.Triggers) > 0:
			k.Triggers = as.Triggers
		case as.Metric != "":
			target := as.Target
			if target == "" {
				target = DefaultAutoscaleTarget
			}
			k.Triggers = []map[string]any{utilizationTrigger(as.Metric, target)}
		default:
			k.Triggers = []map[string]any{utilizationTrigger(DefaultAutoscaleMetric, DefaultAutoscaleTarget)}
		}
	}
	return k
}

// utilizationTrigger builds a KEDA cpu/memory trigger using the v2.17+ shape:
// trigger-level `metricType: Utilization` with `metadata.value`, NOT the
// deprecated `metadata.type` (removed in newer KEDA). value is the utilization
// percentage as a string (e.g. "70").
func utilizationTrigger(metric, value string) map[string]any {
	return map[string]any{
		"type":       metric,
		"metricType": "Utilization",
		"metadata": map[string]any{
			"value": value,
		},
	}
}

func buildServiceMonitor(app appconfig.App) ServiceMonitorValues {
	return ServiceMonitorValues{
		Enabled:      app.MetricsEnabled(),
		Path:         app.Metrics.Path,
		Port:         "http",
		Interval:     "30s",
		ReleaseLabel: DefaultReleaseLabel,
	}
}

func buildPDB(app appconfig.App) PdbValues {
	// Enable a PDB when there is more than one replica (or autoscaling keeps >1).
	enabled := app.Sizing.Replicas > 1
	if app.Sizing.Autoscale.Enabled && app.Sizing.Autoscale.Min > 1 {
		enabled = true
	}
	return PdbValues{Enabled: enabled, MinAvailable: 1}
}

func hostsOf(app appconfig.App) []string {
	var hosts []string
	for _, r := range app.Routes {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	if hosts == nil {
		hosts = []string{}
	}
	return hosts
}

// splitImage prefers the CI-injected image (repo:tag). When image is empty it
// falls back to the deploy.yaml repository with an empty tag (validate/policy
// will reject an empty tag in prod).
func splitImage(image, fallbackRepo string) (repo, tag string) {
	image = strings.TrimSpace(image)
	if image == "" {
		return fallbackRepo, ""
	}
	if at := strings.Index(image, "@"); at >= 0 {
		// digest ref
		return image[:at], image[at+1:]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:]
	}
	return image, ""
}
