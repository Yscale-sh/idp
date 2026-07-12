package appconfig

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidationError is a single structural problem with a deploy.yaml. Field is a
// dotted path (e.g. "db[1].type") so the message points at the offending key.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationErrors aggregates every structural problem found in one pass so a
// developer sees all mistakes at once instead of fixing them one at a time.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	parts := make([]string, len(e))
	for i, v := range e {
		parts[i] = v.Error()
	}
	return strings.Join(parts, "; ")
}

// dns1123Label is the Kubernetes DNS-1123 label rule (used for app/product/
// component/capability names). Mirrors the JSON Schema pattern.
var retentionPattern = regexp.MustCompile(`^[1-9][0-9]*[dh]$`)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// validStoreType / validStorageType / validConnectMode are the closed sets the
// renderer understands. They mirror schemas/deploy.schema.json enums.
var (
	validProfileSet    = setOf(ValidProfiles...)
	validStoreTypeSet  = setOf("postgres", "redis", "mongo")
	validSizeSet       = setOf("minimal", "small", "medium", "large")
	validStorageType   = setOf("r2", "s3")
	validConnectMode   = setOf("publicRoute", "clusterService", "serviceToken")
	validAutoscaleKind = setOf(DefaultAutoscaleK, HTTPScaledObjectK)
)

func setOf(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func keysOf(m map[string]bool) string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	// stable-ish: sort for deterministic messages.
	for i := 0; i < len(ks); i++ {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return strings.Join(ks, "|")
}

// Validate performs structural (schema-equivalent) validation of an App. It is
// environment-agnostic: required fields, name patterns, enum membership, and
// range checks. Environment-aware guardrails (mutable prod tags, approved zones,
// namespace isolation, resource bounds) live in internal/policy.
//
// Validate does NOT mutate the App; call ApplyDefaults first if you want defaults
// reflected in messages. It returns nil or a ValidationErrors.
func (a *App) Validate() error {
	var errs ValidationErrors
	add := func(field, msg string) { errs = append(errs, ValidationError{Field: field, Message: msg}) }

	// app (required, DNS-1123, <=63).
	switch {
	case a.App == "":
		add("app", "is required")
	case len(a.App) > 63:
		add("app", "must be at most 63 characters")
	case !dns1123Label.MatchString(a.App):
		add("app", "must be a DNS-1123 label (lowercase alphanumeric and '-')")
	}

	if a.Product != "" && !dns1123Label.MatchString(a.Product) {
		add("product", "must be a DNS-1123 label")
	}
	if a.Component != "" && !dns1123Label.MatchString(a.Component) {
		add("component", "must be a DNS-1123 label")
	}

	// runtime (required image + valid port).
	if a.Runtime.Image == "" {
		add("runtime.image", "is required")
	} else if strings.Contains(a.Runtime.Image, ":") {
		// The repo carries no tag; CI injects it via --image.
		add("runtime.image", "must be the repository only, without a tag (CI supplies --image)")
	}
	// Port 0 is a WORKER (no inbound service); otherwise a valid TCP port.
	if a.Runtime.Port < 0 || a.Runtime.Port > 65535 {
		add("runtime.port", "must be 0 (worker) or between 1 and 65535")
	}
	if a.IsWorker() && len(a.Routes) > 0 {
		add("routes", "a worker (runtime.port: 0) has no Service and cannot declare routes")
	}

	// logging.retention: a duration Loki accepts in retention_stream ("90d", "12h").
	if r := a.Logging.Retention; r != "" && !retentionPattern.MatchString(r) {
		add("logging.retention", `must be <N>d or <N>h (e.g. "90d", "12h")`)
	}

	// volumes (name + mountPath required; type in nfs|emptyDir|pvc; source per type).
	seenVol := map[string]bool{}
	for i, v := range a.Volumes {
		field := fmt.Sprintf("volumes[%d]", i)
		if v.Name == "" {
			add(field+".name", "is required")
		} else if seenVol[v.Name] {
			add(field+".name", "duplicate volume name "+v.Name)
		}
		seenVol[v.Name] = true
		if v.MountPath == "" {
			add(field+".mountPath", "is required")
		}
		switch v.ResolvedType() {
		case "nfs":
			if v.Server == "" || v.Path == "" {
				add(field, "nfs volume requires server and path")
			}
		case "pvc":
			if v.Claim == "" && v.Size == "" {
				add(field, "pvc volume requires either claim (reference an existing PVC) or size (provision a new one)")
			}
			if v.Claim != "" && v.Size != "" {
				add(field, "pvc volume: set claim OR size, not both")
			}
		case "emptyDir":
			// no source fields
		case "secret":
			if v.Secret == "" {
				add(field, "secret volume requires secret (the Kubernetes Secret name)")
			}
		default:
			add(field+".type", "must be one of nfs|emptyDir|pvc|secret")
		}
	}

	// expose (LAN MetalLB) — on-prem only; the renderer/policy enforce the backend.
	if a.Expose != nil && a.Expose.LAN && a.IsWorker() {
		add("expose", "a worker (runtime.port: 0) has no Service to expose")
	}

	// sizing.
	if a.Sizing.Profile != "" && !validProfileSet[a.Sizing.Profile] {
		add("sizing.profile", "must be one of "+keysOf(validProfileSet))
	}
	if a.Sizing.Replicas != nil && *a.Sizing.Replicas < 0 {
		add("sizing.replicas", "must be >= 0")
	}
	a.validateAutoscale(add)

	// routes.
	for i, r := range a.Routes {
		if r.Host == "" {
			add(fmt.Sprintf("routes[%d].host", i), "is required")
		}
	}

	// db / cache.
	a.validateStores("db", a.DB, add)
	a.validateStores("cache", a.Cache, add)

	// storage.
	seenStorage := map[string]bool{}
	for i, s := range a.Storage {
		field := fmt.Sprintf("storage[%d]", i)
		if s.Name == "" {
			add(field+".name", "is required")
		} else if seenStorage[s.Name] {
			add(field+".name", "duplicate storage name "+s.Name)
		}
		seenStorage[s.Name] = true
		if s.Type != "" && !validStorageType[s.Type] {
			add(field+".type", "must be one of "+keysOf(validStorageType))
		}
	}

	// metrics path.
	if a.Metrics.Path != "" && !strings.HasPrefix(a.Metrics.Path, "/") {
		add("metrics.path", "must start with '/'")
	}

	// connectsTo.
	for i, c := range a.ConnectsTo {
		field := fmt.Sprintf("connectsTo[%d]", i)
		if c.Env == "" {
			add(field+".env", "is required")
		}
		if c.App == "" && c.Component == "" {
			add(field, "must set either app or component")
		}
		if c.App != "" && !dns1123Label.MatchString(c.App) {
			add(field+".app", "must be a DNS-1123 label")
		}
		if c.Component != "" && !dns1123Label.MatchString(c.Component) {
			add(field+".component", "must be a DNS-1123 label")
		}
		if c.Mode != "" && !validConnectMode[c.Mode] {
			add(field+".mode", "must be one of "+keysOf(validConnectMode))
		}
	}

	if len(errs) > 0 {
		return errs
	}
	return nil
}

func (a *App) validateAutoscale(add func(field, msg string)) {
	as := a.Sizing.Autoscale
	if !as.Enabled {
		return
	}
	if as.Kind != "" && !validAutoscaleKind[as.Kind] {
		add("sizing.autoscale.kind", "must be one of "+keysOf(validAutoscaleKind))
	}
	if as.Min < 0 {
		add("sizing.autoscale.min", "must be >= 0")
	}
	if as.Max < 0 {
		add("sizing.autoscale.max", "must be >= 0")
	}
	if as.Max > 0 && as.Min > as.Max {
		add("sizing.autoscale", fmt.Sprintf("min (%d) must be <= max (%d)", as.Min, as.Max))
	}
	// Scale-to-zero (min 0) is only meaningful for HTTPScaledObject.
	if as.Min == 0 && as.Kind == DefaultAutoscaleK {
		add("sizing.autoscale.min", "min 0 (scale-to-zero) requires kind HTTPScaledObject")
	}
}

func (a *App) validateStores(class string, stores []DataStore, add func(field, msg string)) {
	seen := map[string]bool{}
	for i, s := range stores {
		field := fmt.Sprintf("%s[%d]", class, i)
		if s.Name == "" {
			add(field+".name", "is required")
		} else if seen[s.Name] {
			add(field+".name", fmt.Sprintf("duplicate %s name %s", class, s.Name))
		}
		seen[s.Name] = true
		if s.Type != "" && !validStoreTypeSet[s.Type] {
			add(field+".type", "must be one of "+keysOf(validStoreTypeSet))
		}
		if s.Size != "" && !validSizeSet[s.Size] {
			add(field+".size", "must be one of "+keysOf(validSizeSet))
		}
	}
}
