package render

import (
	"fmt"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
)

// EnvVar is one rendered environment variable. Value-style vars set Value;
// secret-backed vars set SecretKey (the key within the runtime Secret) — but the
// app chart's standard path is envFrom the whole runtime Secret, so the secret
// *keys* here are documented for the secrets package and for the plan summary,
// not re-rendered into the Deployment.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// TierAEnv builds the Tier-A platform-injected vars (ENV.md). IMAGE_NAME is
// intentionally NOT included (Helm sets the image). DEPLOY_TIME is supplied by
// CI; when empty the renderer leaves it blank for Flux/CI to stamp.
func TierAEnv(app appconfig.App, env string, obs clusterenv.Observability, deployTime string) map[string]string {
	m := map[string]string{
		"ENVIRONMENT":     env,
		"LOKI_URL":        obs.LokiURL,
		"CONSOLE_LOGGING": obs.ConsoleLoggingValue(),
		"DEPLOY_TIME":     deployTime,
		"PORT":            fmt.Sprintf("%d", app.Runtime.Port),
	}
	// Only inject the OTLP endpoint when the env actually provides a collector —
	// an empty value makes OTEL SDKs fall back to localhost:4317 and retry-spam.
	if obs.OTLPEndpoint != "" {
		m["OTEL_EXPORTER_OTLP_ENDPOINT"] = obs.OTLPEndpoint
	}
	// PostHog: one shared (public) project token injected into every app so analytics
	// is wired by default and rotates from a single swap in the cluster's observability
	// config. Only inject when set, so non-analytics envs stay clean. Server apps read
	// POSTHOG_PROJECT_TOKEN directly; frontends read it at runtime via their /config shim.
	if obs.PostHogToken != "" {
		m["POSTHOG_PROJECT_TOKEN"] = obs.PostHogToken
		m["POSTHOG_HOST"] = obs.PostHogHostValue()
	}
	return m
}

// SecretKeys returns the full ordered list of secret-backed env keys this app
// expects in its runtime Secret, including data-store URLs and shared-group
// keys. This drives both the plan summary and the secrets package's
// ExternalSecret generation. It is the canonical "what secrets does this app
// need" answer.
//
// Data-store URL keys (Tier D) follow the naming rule: the first/default entry
// of each class gets the bare alias (DATABASE_URL / REDIS_URL) PLUS the prefixed
// form; every named entry always gets the prefixed form.
func DataStoreEnvKeys(app appconfig.App) []string {
	var keys []string
	for i, s := range app.DB {
		keys = append(keys, storeKeys([]appconfig.DataStore{s}, "DATABASE_URL", i == 0)...)
	}
	for i, s := range app.Cache {
		keys = append(keys, storeKeys([]appconfig.DataStore{s}, "REDIS_URL", i == 0)...)
	}
	return keys
}

// storeKeys returns the env-var key(s) one store contributes. The first/default
// entry of its class (isDefault) gets the bare alias (DATABASE_URL/REDIS_URL)
// PLUS the prefixed <NAME>_<SUFFIX> form; every other entry gets only the
// prefixed form so multiple stores never collide.
func storeKeys(stores []appconfig.DataStore, suffix string, isDefault bool) []string {
	var keys []string
	for _, s := range stores {
		prefixed := appconfig.EnvPrefix(s.Name) + "_" + suffix
		if isDefault {
			keys = append(keys, suffix, prefixed)
		} else {
			keys = append(keys, prefixed)
		}
	}
	return keys
}

// StorageEnvKeys returns the storage-related env keys (Tier C-ish: object store
// creds). Each bucket contributes <NAME>_{BUCKET,ENDPOINT,ACCESS_KEY_ID,
// SECRET_ACCESS_KEY}.
func StorageEnvKeys(app appconfig.App) []string {
	var keys []string
	for _, s := range app.Storage {
		p := appconfig.EnvPrefix(s.Name)
		keys = append(keys,
			p+"_BUCKET",
			p+"_ENDPOINT",
			p+"_ACCESS_KEY_ID",
			p+"_SECRET_ACCESS_KEY",
		)
	}
	return keys
}
