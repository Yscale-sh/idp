package render

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

func managedR2Profile() clusterenv.StorageProfile {
	return clusterenv.StorageProfile{
		APIVersion:      "r2.upjet-cloudflare.m.upbound.io/v1alpha1",
		Kind:            "Bucket",
		Namespace:       "crossplane-system",
		BucketNameField: "name",
		Endpoint:        "https://account-id.r2.cloudflarestorage.com",
		ProviderConfigRef: clusterenv.ProviderConfigReference{
			Name: "default",
			Kind: "ProviderConfig",
		},
		ForProvider: map[string]any{"accountId": "account-id-is-not-secret"},
	}
}

func boolPointer(v bool) *bool { return &v }

func TestBuildStoreReleasesUsesResolvedStatefulStoreSeam(t *testing.T) {
	app := appconfig.App{
		App:   "checkout",
		DB:    []appconfig.DataStore{{Name: "primary", Type: "postgres"}},
		Cache: []appconfig.DataStore{{Name: "sessions", Type: "redis"}},
	}
	local := &clusterenv.Config{Secrets: clusterenv.SecretsConfig{Backend: clusterenv.BackendLocal}}
	if got := BuildStoreReleases(app, "dev", local); len(got) != 2 {
		t.Fatalf("local stateful environment rendered %d stores, want postgres and redis", len(got))
	}
	ssm := &clusterenv.Config{Secrets: clusterenv.SecretsConfig{Backend: clusterenv.BackendSSM}}
	if got := BuildStoreReleases(app, "prod", ssm); len(got) != 0 {
		t.Fatalf("ssm environment with an unset seam rendered %d unused local stores", len(got))
	}
}

func TestBuildBucketsRendersConcreteRetainedResource(t *testing.T) {
	profile := managedR2Profile()
	c := &clusterenv.Config{StorageProfiles: map[string]clusterenv.StorageProfile{"r2": profile}}
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Bucket: "media-prod-uploads", Provision: boolPointer(true),
	}}}

	got, err := BuildBuckets(app, "prod", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("buckets = %d, want 1", len(got))
	}
	b := got[0]
	if b["apiVersion"] != profile.APIVersion || b["kind"] != "Bucket" {
		t.Fatalf("bucket GVK = %v %v", b["apiVersion"], b["kind"])
	}
	metadata := b["metadata"].(map[string]any)
	if metadata["name"] != "media-prod-uploads" || metadata["namespace"] != "crossplane-system" {
		t.Fatalf("bucket metadata = %+v", metadata)
	}
	annotations := metadata["annotations"].(map[string]string)
	if annotations["crossplane.io/external-name"] != "media-prod-uploads" {
		t.Fatalf("external name = %q", annotations["crossplane.io/external-name"])
	}
	spec := b["spec"].(map[string]any)
	forProvider := spec["forProvider"].(map[string]any)
	if forProvider["name"] != "media-prod-uploads" || forProvider["accountId"] != "account-id-is-not-secret" {
		t.Fatalf("forProvider = %+v", forProvider)
	}
	for _, policy := range spec["managementPolicies"].([]string) {
		if policy == "Delete" || policy == "*" {
			t.Fatalf("managementPolicies must retain the external bucket, got %v", spec["managementPolicies"])
		}
	}
	if _, mutated := profile.ForProvider["name"]; mutated {
		t.Fatal("render mutated the environment profile")
	}

	second, err := BuildBuckets(app, "prod", c)
	if err != nil {
		t.Fatal(err)
	}
	firstYAML, _ := yaml.Marshal(got)
	secondYAML, _ := yaml.Marshal(second)
	if string(firstYAML) != string(secondYAML) {
		t.Fatalf("separate renders differ:\n%s\n---\n%s", firstYAML, secondYAML)
	}
}

func TestStorageWiringMatchesDerivedManagedBucket(t *testing.T) {
	c := &clusterenv.Config{StorageProfiles: map[string]clusterenv.StorageProfile{"r2": managedR2Profile()}}
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Provision: boolPointer(true),
	}}}
	buckets, err := BuildBuckets(app, "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	metadata := buckets[0]["metadata"].(map[string]any)
	env := buildAppEnv(app, "dev", c)
	if env["UPLOADS_BUCKET"] != metadata["name"] {
		t.Fatalf("app bucket %q does not match managed resource %q", env["UPLOADS_BUCKET"], metadata["name"])
	}
	if env["UPLOADS_ENDPOINT"] != managedR2Profile().Endpoint {
		t.Fatalf("endpoint = %q", env["UPLOADS_ENDPOINT"])
	}
	if len(metadata["name"].(string)) > 63 {
		t.Fatalf("managed name is not a DNS label: %q", metadata["name"])
	}
}

func TestExistingBucketWiringDoesNotCreateClaim(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Bucket: "existing-bucket",
	}}}
	got, err := BuildBuckets(app, "prod", &clusterenv.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("default provision:false rendered %d managed resources", len(got))
	}
	if env := buildAppEnv(app, "prod", nil); env["UPLOADS_BUCKET"] != "existing-bucket" {
		t.Fatalf("existing bucket wiring = %v", env)
	}
}

func TestBuildBucketsFailsClosed(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Provision: boolPointer(true),
	}}}
	if _, err := BuildBuckets(app, "prod", &clusterenv.Config{}); err == nil || !strings.Contains(err.Error(), "no storage profile") {
		t.Fatalf("missing provider profile error = %v", err)
	}
	c := &clusterenv.Config{StorageProfiles: map[string]clusterenv.StorageProfile{"r2": managedR2Profile()}}
	app.Storage[0].Public = true
	if _, err := BuildBuckets(app, "prod", c); err == nil || !strings.Contains(err.Error(), "public bucket policy") {
		t.Fatalf("unsupported public provisioning error = %v", err)
	}
}

func TestMultiComponentBucketProvisionedOnce(t *testing.T) {
	app := appconfig.App{
		App: "media",
		Storage: []appconfig.Storage{{
			Name: "uploads", Type: "r2", Provision: boolPointer(true),
		}},
		Components: []appconfig.Component{{Component: "api"}, {Component: "worker"}},
	}
	expanded := app.Expand()
	if !expanded[0].Storage[0].Provisioned() || expanded[1].Storage[0].Provisioned() {
		t.Fatalf("component ownership = first:%v second:%v", expanded[0].Storage[0].Provisioned(), expanded[1].Storage[0].Provisioned())
	}
}

func TestBucketEntryRoundTripPreservesProviderFields(t *testing.T) {
	input := `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: platform, namespace: flux-system}
spec:
  values:
    env: prod
    source: {name: flux-system, namespace: flux-system}
    modules: []
    apps:
      - name: media
        namespace: media-prod
        releaseName: media
        values: {}
        buckets:
          - namespace: crossplane-system
            releaseName: media-prod-uploads-bucket
            resource:
              apiVersion: example.io/v1
              kind: Bucket
              metadata: {name: media-prod-uploads}
              spec:
                initProvider: {providerField: keep-me}
                forProvider: {name: media-prod-uploads}
`
	platform, err := ParsePlatform([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(platform)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "providerField: keep-me") {
		t.Fatalf("provider-specific field was dropped:\n%s", out)
	}
}

func TestToAppEntryCarriesBucketIntoIsolatedRelease(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Provision: boolPointer(true),
	}}}
	buckets, err := BuildBuckets(app, "prod", &clusterenv.Config{
		StorageProfiles: map[string]clusterenv.StorageProfile{"r2": managedR2Profile()},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Result{
		App: app, Env: "prod", Buckets: buckets,
		HelmRelease: HelmRelease{Metadata: FluxMetadata{Namespace: "flux-system"}},
	}
	entry := result.ToAppEntry()
	if len(entry.Buckets) != 1 {
		t.Fatalf("umbrella entry buckets = %d", len(entry.Buckets))
	}
	if entry.Buckets[0].ReleaseName != "media-prod-uploads-bucket" || entry.Buckets[0].Namespace != "crossplane-system" {
		t.Fatalf("isolated bucket release = %+v", entry.Buckets[0])
	}
	if entry.Buckets[0].Resource["kind"] != "Bucket" {
		t.Fatalf("managed resource lost: %+v", entry.Buckets[0].Resource)
	}
}
