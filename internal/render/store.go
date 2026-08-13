package render

import (
	"fmt"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// devPostgresChartPath / devRedisChartPath are the in-repo paths to the per-app
// dev data-store charts, referenced by the GitRepository source.
const (
	devPostgresChartPath = "./charts/infra/dev-postgres"
	devRedisChartPath    = "./charts/infra/dev-redis"
)

// StoreRelease is one rendered per-app data-store Flux HelmRelease (its own
// namespace, its own chart). It pairs the HelmRelease with the canonical output
// filename stem so the caller writes it to environments/<env>/apps/<file>.yaml.
type StoreRelease struct {
	// FileStem is the filename stem under environments/<env>/apps/ (no .yaml),
	// e.g. "carshowdb-postgres". The FluxInstance Kustomization applies the whole
	// environments/<env> tree, so any file dropped there is picked up.
	FileStem string
	// Tool is the store engine/purpose ("postgres", "redis"), for the plan summary.
	Tool string
	// Namespace is the dedicated store namespace <app>-<env>-<tool>.
	Namespace   string
	HelmRelease HelmRelease
}

// BuildStoreReleases renders the dedicated per-app data-store Flux HelmReleases
// for an app in an env. Today this is the DEV per-app Postgres: when the env's
// backend is local (dev) and the app declares one or more db: postgres stores,
// each gets its OWN dev-postgres HelmRelease (chart ./charts/infra/dev-postgres)
// with targetNamespace <app>-<env>-postgres, the per-app database name, a DEV
// PLACEHOLDER password, the storage-node node pin, and install.createNamespace=true.
//
// In prod (backend=ssm) NO per-app Postgres is provisioned — the DB is
// external/managed and DATABASE_URL comes from SSM — so this returns nil.
// Redis dev provisioning lands here too once a dev-redis chart exists.
func BuildStoreReleases(app appconfig.App, env string, c *clusterenv.Config) []StoreRelease {
	if c != nil && !c.EffectiveSeams().StatefulStores {
		return nil
	}
	var out []StoreRelease

	// Per-app dev Postgres for each declared postgres db. The first/default db
	// owns the bare <app>-postgres name/namespace; a second postgres store
	// disambiguates with its store name (<app>-postgres-<name>). A store with
	// provision: false is SHARED from a sibling component — its URL is still wired,
	// but it is not stood up here (so api+scanner share one Postgres).
	pgSeen := 0
	for _, s := range app.DB {
		if s.Type != appconfig.DefaultDBType || !s.Provisioned() {
			continue
		}
		secondary := pgSeen > 0
		pgSeen++
		out = append(out, buildDevPostgresRelease(app, env, c, s.Name, secondary))
	}

	// Per-app dev Redis for each declared redis cache (same sharing semantics).
	redisSeen := 0
	for _, s := range app.Cache {
		if s.Type != appconfig.DefaultCacheType || !s.Provisioned() {
			continue
		}
		secondary := redisSeen > 0
		redisSeen++
		out = append(out, buildDevRedisRelease(app, env, c, s.Name, secondary))
	}
	return out
}

// buildDevRedisRelease renders one dedicated dev-redis Flux HelmRelease for the
// app's declared redis cache (chart ./charts/infra/dev-redis), targetNamespace
// <app>-<env>-redis. Mirrors buildDevPostgresRelease; the dev Redis is an
// in-cluster, no-persistence pub/sub bus (no auth), matching clusterenv.DevRedisURL.
func buildDevRedisRelease(app appconfig.App, env string, c *clusterenv.Config, storeName string, secondary bool) StoreRelease {
	sourceName, sourceNS := fluxSource(c)

	release := clusterenv.DevRedisReleaseName(app.App)
	ns := app.StoreNamespace(env, clusterenv.DevRedisTool, storeName, secondary)
	fileStem := release
	if secondary && storeName != "" {
		release = release + "-" + appconfig.SanitizeDNSLabel(storeName)
		fileStem = release
	}

	values := map[string]any{
		"service":     map[string]any{"port": 6379},
		"persistence": map[string]any{"enabled": false},
	}
	if clusterenv.DevPostgresNode != "" {
		values["nodeSelector"] = map[string]any{"kubernetes.io/hostname": clusterenv.DevPostgresNode}
	}

	hr := HelmRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: FluxMetadata{
			Name:      release,
			Namespace: sourceNS,
			Labels:    storeLabels(app.App, env, clusterenv.DevRedisTool),
		},
		Spec: HelmReleaseSpec{
			Interval:         fluxInterval,
			ReleaseName:      release,
			TargetNamespace:  ns,
			StorageNamespace: ns,
			Install:          InstallSpec{CreateNamespace: true, Remediation: &RemediationSpec{Retries: remediationRetries}},
			Upgrade:          UpgradeSpec{Remediation: &RemediationSpec{Retries: remediationRetries}},
			Chart: ChartTemplate{Spec: ChartSpec{
				Chart:             devRedisChartPath,
				SourceRef:         SourceRef{Kind: sourceKindGitRepo, Name: sourceName, Namespace: sourceNS},
				ReconcileStrategy: reconcileRevision,
			}},
			Values: values,
		},
	}

	return StoreRelease{FileStem: fileStem, Tool: clusterenv.DevRedisTool, Namespace: ns, HelmRelease: hr}
}

// buildDevPostgresRelease renders one dedicated dev-postgres Flux HelmRelease for
// the app's declared postgres store. release/service/namespace are derived from
// the app (and the store name for a secondary store); the inline Helm values pin
// the per-app database, the dev placeholder password, the storage-node node, and
// the local-path PVC — mirroring what clusterenv.DevDatabaseURL assumes so the
// app's rendered DATABASE_URL points at exactly what this chart provisions.
func buildDevPostgresRelease(app appconfig.App, env string, c *clusterenv.Config, storeName string, secondary bool) StoreRelease {
	sourceName, sourceNS := fluxSource(c)

	release := clusterenv.DevPostgresReleaseName(app.App)
	ns := app.StoreNamespace(env, clusterenv.DevPostgresTool, storeName, secondary)
	fileStem := release
	if secondary && storeName != "" {
		release = release + "-" + appconfig.SanitizeDNSLabel(storeName)
		fileStem = release
	}

	labels := storeLabels(app.App, env, clusterenv.DevPostgresTool)
	values := map[string]any{
		"auth": map[string]any{
			"database": clusterenv.DevPostgresDatabase(app.App),
			"username": clusterenv.DevPostgresDefaultUser,
			"password": clusterenv.DevPostgresDefaultPassword,
		},
		"service": map[string]any{"port": 5432},
		// Pin to the cluster baseline node so the local-path PVC reschedules onto
		// the box that physically holds the data volume.
		"persistence": map[string]any{
			"enabled":      true,
			"storageClass": "local-path",
			"size":         "8Gi",
		},
	}
	// Pin Postgres to a baseline node only when one is configured. In dev the pin is
	// empty so the scheduler can place it on any node that can pull + run the image
	// (the cluster's bare-metal node currently can't reach external registries).
	if clusterenv.DevPostgresNode != "" {
		values["nodeSelector"] = map[string]any{"kubernetes.io/hostname": clusterenv.DevPostgresNode}
	}

	hr := HelmRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: FluxMetadata{
			Name:      release,
			Namespace: sourceNS,
			Labels:    labels,
		},
		Spec: HelmReleaseSpec{
			Interval:         fluxInterval,
			ReleaseName:      release,
			TargetNamespace:  ns,
			StorageNamespace: ns,
			Install: InstallSpec{
				CreateNamespace: true,
				Remediation:     &RemediationSpec{Retries: remediationRetries},
			},
			Upgrade: UpgradeSpec{
				Remediation: &RemediationSpec{Retries: remediationRetries},
			},
			Chart: ChartTemplate{
				Spec: ChartSpec{
					Chart: devPostgresChartPath,
					SourceRef: SourceRef{
						Kind:      sourceKindGitRepo,
						Name:      sourceName,
						Namespace: sourceNS,
					},
					ReconcileStrategy: reconcileRevision,
				},
			},
			Values: values,
		},
	}

	return StoreRelease{
		FileStem:    fileStem,
		Tool:        clusterenv.DevPostgresTool,
		Namespace:   ns,
		HelmRelease: hr,
	}
}

// storeLabels is the label set stamped on a per-app store HelmRelease. It mirrors
// the app label set but marks the purpose as the store tool (component=<tool>).
func storeLabels(app, env, tool string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     app + "-" + tool,
		"app.kubernetes.io/instance": app + "-" + tool,
		"platform/app":               app,
		"platform/env":               env,
		"platform/component":         tool,
		"platform/managed-by":        "platformctl",
	}
}

// ManagedResource is the LEGACY Crossplane managed resource an older idpctl
// persisted under an umbrella bucket entry. It stays unstructured so a
// provider-specific field survives an unrelated app's read/modify/write cycle;
// nothing renders it any more (see BucketEntry.Resource / WritePlatform).
type ManagedResource map[string]any

// BucketValues is the charts/infra/bucket-provisioner values for one bucket.
// Everything here is non-secret: the credentials block names remote entries,
// it never carries their values.
type BucketValues struct {
	Bucket      string                 `json:"bucket"`
	Endpoint    string                 `json:"endpoint"`
	Region      string                 `json:"region,omitempty"`
	PathStyle   bool                   `json:"pathStyle,omitempty"`
	Image       string                 `json:"image"`
	Credentials BucketCredentialValues `json:"credentials"`
	Labels      map[string]string      `json:"labels,omitempty"`
}

// BucketCredentialValues locates one S3-compatible key pair in the env's secret
// backend. REFERENCES ONLY — the values are resolved in-cluster by
// external-secrets, so the rendered umbrella stays safe to commit.
type BucketCredentialValues struct {
	StoreRef        StoreRefValues        `json:"storeRef"`
	RefreshInterval string                `json:"refreshInterval,omitempty"`
	AccessKeyID     RemoteSecretRefValues `json:"accessKeyID"`
	SecretAccessKey RemoteSecretRefValues `json:"secretAccessKey"`
}

// RemoteSecretRefValues is one entry in the secret backend: a key plus, for
// backends whose key addresses a multi-entry object, the property within it.
type RemoteSecretRefValues struct {
	Key      string `json:"key"`
	Property string `json:"property,omitempty"`
}

// BuildBuckets renders one isolated bucket-provisioner release per explicitly
// provisioned bucket. It is deterministic and performs no cloud or Kubernetes
// writes — the release's Helm hook does the creating, in-cluster.
//
// Buckets are provider-neutral: every supported backend (MinIO, R2, S3) speaks
// the S3 API, so the env's storage profile supplies endpoint/region/addressing
// and the credential references, and the same chart serves all of them.
func BuildBuckets(app appconfig.App, env string, c *clusterenv.Config) ([]BucketEntry, error) {
	var out []BucketEntry
	for _, storage := range app.Storage {
		if !storage.Provisioned() {
			continue
		}

		profile, ok := StorageProfileFor(c, storage)
		if !ok {
			return nil, fmt.Errorf("app %q requests storage bucket %q (type %q) with provision=true, but env %q has no storage profile for that type", app.App, storage.Name, storage.Type, env)
		}
		if storage.Public {
			return nil, fmt.Errorf("app %q requests public storage bucket %q, but public bucket policy provisioning is not supported; use public=false or provision=false", app.App, storage.Name)
		}

		name := appconfig.SanitizeDNSLabel(app.App + "-" + env + "-" + storage.Name)
		out = append(out, BucketEntry{
			Namespace:   profile.Namespace,
			ReleaseName: appconfig.SanitizeDNSLabel(name + "-bucket"),
			Values: BucketValues{
				Bucket:      StorageBucketName(app, env, storage),
				Endpoint:    profile.Endpoint,
				Region:      profile.Region,
				PathStyle:   profile.PathStyle,
				Image:       profile.Image,
				Credentials: BucketCredentials(profile),
				Labels: map[string]string{
					"platform/app":        app.App,
					"platform/env":        env,
					"platform/component":  "storage",
					"platform/managed-by": "platformctl",
				},
			},
		})
	}
	return out, nil
}

// StorageProfileFor resolves the env profile backing a declared bucket, if the
// env configures one for that storage type.
func StorageProfileFor(c *clusterenv.Config, storage appconfig.Storage) (clusterenv.StorageProfile, bool) {
	if c == nil {
		return clusterenv.StorageProfile{}, false
	}
	profile, ok := c.StorageProfiles[storage.Type]
	return profile, ok
}

// BucketCredentials projects a profile's credential references into chart
// values. Shared by the provisioner release and each consuming app, so both
// pull the same key pair from the same store.
func BucketCredentials(profile clusterenv.StorageProfile) BucketCredentialValues {
	cred := profile.Credentials
	return BucketCredentialValues{
		StoreRef: StoreRefValues{
			Name: cred.StoreRef.Name,
			Kind: cred.StoreRefKind(),
		},
		RefreshInterval: cred.RefreshInterval,
		AccessKeyID:     RemoteSecretRefValues{Key: cred.AccessKeyID.Key, Property: cred.AccessKeyID.Property},
		SecretAccessKey: RemoteSecretRefValues{Key: cred.SecretAccessKey.Key, Property: cred.SecretAccessKey.Property},
	}
}

// StorageBucketName resolves the physical bucket name shared by the provisioner
// release and the application's non-secret <NAME>_BUCKET variable.
func StorageBucketName(app appconfig.App, env string, storage appconfig.Storage) string {
	if storage.Bucket != "" {
		return storage.Bucket
	}
	return appconfig.SanitizeDNSLabel(app.App + "-" + env + "-" + storage.Name)
}
