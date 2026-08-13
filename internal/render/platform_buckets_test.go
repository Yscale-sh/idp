package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// legacyUmbrella is an umbrella written by the Crossplane-era idpctl: the bucket
// entry carries a `resource:` block instead of provisioner values.
const legacyUmbrella = `
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

func TestParsePlatformPreservesLegacyManagedResource(t *testing.T) {
	platform, err := ParsePlatform([]byte(legacyUmbrella))
	if err != nil {
		t.Fatal(err)
	}
	buckets := platform.Spec.Values.Apps[0].Buckets
	if len(buckets) != 1 || len(buckets[0].Resource) == 0 {
		t.Fatalf("legacy managed resource dropped on parse: %+v", buckets)
	}
	out, err := yaml.Marshal(platform)
	if err != nil {
		t.Fatal(err)
	}
	// Parsing must not quietly prune a persisted resource — the operator has to
	// be able to see what is still out there before anything is removed.
	if !strings.Contains(string(out), "providerField: keep-me") {
		t.Fatalf("provider-specific field was dropped:\n%s", out)
	}
}

func TestWritePlatformRefusesLegacyManagedResource(t *testing.T) {
	platform, err := ParsePlatform([]byte(legacyUmbrella))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	path, err := WritePlatform(root, "prod", platform)
	if err == nil {
		t.Fatal("WritePlatform accepted a Crossplane-era bucket entry")
	}
	if path != "" {
		t.Fatalf("refused write still returned a path: %q", path)
	}
	// The error has to say WHICH entry and WHAT to run, or the operator is stuck.
	for _, want := range []string{"media/media-prod-uploads-bucket", "idpctl render", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("migration error missing %q: %v", want, err)
		}
	}
	// Fail CLOSED: nothing on disk was touched.
	if _, statErr := os.Stat(filepath.Join(root, "clusters", "prod", "platform.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("refused write still created platform.yaml (%v)", statErr)
	}
}

func TestWritePlatformAcceptsMigratedBucketEntry(t *testing.T) {
	platform, err := ParsePlatform([]byte(legacyUmbrella))
	if err != nil {
		t.Fatal(err)
	}
	// Re-rendering the owning app is what clears the legacy block.
	platform.Spec.Values.Apps[0].Buckets = []BucketEntry{{
		Namespace:   "platform-storage",
		ReleaseName: "media-prod-uploads-bucket",
		Values: BucketValues{
			Bucket:   "media-prod-uploads",
			Endpoint: "https://minio.example.invalid",
			Image:    "minio/mc@sha256:" + strings.Repeat("a", 64),
		},
	}}
	root := t.TempDir()
	path, err := WritePlatform(root, "prod", platform)
	if err != nil {
		t.Fatalf("migrated umbrella rejected: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "resource:") {
		t.Fatalf("migrated umbrella still carries a managed resource:\n%s", body)
	}
	if !strings.Contains(string(body), "media-prod-uploads") {
		t.Fatalf("migrated umbrella lost the bucket:\n%s", body)
	}
}

func TestParsePlatformPreservesUnknownProvisionerValues(t *testing.T) {
	input := strings.Replace(legacyUmbrella, `resource:
              apiVersion: example.io/v1
              kind: Bucket
              metadata: {name: media-prod-uploads}
              spec:
                initProvider: {providerField: keep-me}
                forProvider: {name: media-prod-uploads}`, `values:
              bucket: media-prod-uploads
              endpoint: https://minio.example.invalid
              image: minio/mc@sha256:`+strings.Repeat("a", 64)+`
              futureProviderOption: keep-me`, 1)
	platform, err := ParsePlatform([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(platform)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "futureProviderOption: keep-me") {
		t.Fatalf("unknown provisioner value was dropped:\n%s", out)
	}
}
