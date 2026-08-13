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

// ManagedResource remains unstructured so provider-specific fields survive an
// unrelated app's umbrella read/modify/write cycle.
type ManagedResource map[string]any

// BuildBuckets renders Crossplane managed resources for explicitly provisioned
// object storage. It is deterministic and performs no cloud or Kubernetes writes.
func BuildBuckets(app appconfig.App, env string, c *clusterenv.Config) ([]ManagedResource, error) {
	var out []ManagedResource
	for _, storage := range app.Storage {
		if !storage.Provisioned() {
			continue
		}

		var profile clusterenv.StorageProfile
		var ok bool
		if c != nil {
			profile, ok = c.StorageProfiles[storage.Type]
		}
		if !ok {
			return nil, fmt.Errorf("app %q requests storage bucket %q (type %q) with provision=true, but env %q has no storage profile for that type", app.App, storage.Name, storage.Type, env)
		}
		if storage.Public {
			return nil, fmt.Errorf("app %q requests public storage bucket %q, but public bucket policy provisioning is not supported; use public=false or provision=false", app.App, storage.Name)
		}

		name := appconfig.SanitizeDNSLabel(app.App + "-" + env + "-" + storage.Name)
		physicalName := StorageBucketName(app, env, storage)
		forProvider := make(map[string]any, len(profile.ForProvider)+1)
		for key, value := range profile.ForProvider {
			forProvider[key] = value
		}
		if profile.BucketNameField != "" {
			forProvider[profile.BucketNameField] = physicalName
		}

		metadata := map[string]any{
			"name": name,
			"annotations": map[string]string{
				"crossplane.io/external-name": physicalName,
			},
			"labels": map[string]string{
				"app.kubernetes.io/name":     name,
				"app.kubernetes.io/instance": name,
				"platform/app":               app.App,
				"platform/env":               env,
				"platform/component":         "storage",
				"platform/managed-by":        "platformctl",
			},
		}
		if profile.Namespace != "" {
			metadata["namespace"] = profile.Namespace
		}
		providerConfigRef := map[string]any{"name": profile.ProviderConfigRef.Name}
		if profile.ProviderConfigRef.Kind != "" {
			providerConfigRef["kind"] = profile.ProviderConfigRef.Kind
		}
		out = append(out, ManagedResource{
			"apiVersion": profile.APIVersion,
			"kind":       profile.Kind,
			"metadata":   metadata,
			"spec": map[string]any{
				"providerConfigRef":  providerConfigRef,
				"managementPolicies": []string{"Create", "Observe", "Update", "LateInitialize"},
				"forProvider":        forProvider,
			},
		})
	}
	return out, nil
}

// StorageBucketName resolves the physical bucket name shared by the managed
// resource and the application's non-secret <NAME>_BUCKET variable.
func StorageBucketName(app appconfig.App, env string, storage appconfig.Storage) string {
	if storage.Bucket != "" {
		return storage.Bucket
	}
	return appconfig.SanitizeDNSLabel(app.App + "-" + env + "-" + storage.Name)
}
