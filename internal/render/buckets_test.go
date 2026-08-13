package render

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

// minioProfile is a provider-neutral S3 profile. PLACEHOLDERS ONLY: it names no
// real endpoint, account, or credential path.
func minioProfile() clusterenv.StorageProfile {
	return clusterenv.StorageProfile{
		Endpoint:  "https://minio.example.invalid",
		PathStyle: true,
		Namespace: "platform-storage",
		Image:     "minio/mc@sha256:" + strings.Repeat("a", 64),
		Credentials: clusterenv.StorageCredentials{
			StoreRef:        clusterenv.StoreReference{Name: "platform-local", Kind: "ClusterSecretStore"},
			RefreshInterval: "1h",
			AccessKeyID:     clusterenv.RemoteSecretRef{Key: "example-object-storage", Property: "ACCESS_KEY_ID"},
			SecretAccessKey: clusterenv.RemoteSecretRef{Key: "example-object-storage", Property: "SECRET_ACCESS_KEY"},
		},
	}
}

// r2Profile is the virtual-host-addressed, regioned variant (Cloudflare R2 /
// AWS S3 shape) — same chart, different profile fields.
func r2Profile() clusterenv.StorageProfile {
	p := minioProfile()
	p.Endpoint = "https://ACCOUNT_ID.r2.cloudflarestorage.com"
	p.Region = "auto"
	p.PathStyle = false
	return p
}

func profileConfig(profiles map[string]clusterenv.StorageProfile) *clusterenv.Config {
	return &clusterenv.Config{StorageProfiles: profiles}
}

func boolPointer(v bool) *bool { return &v }

func builtBucketValues(t *testing.T, entry BucketEntry) BucketValues {
	t.Helper()
	values, ok := entry.Values.(BucketValues)
	if !ok {
		t.Fatalf("built bucket values type = %T, want BucketValues", entry.Values)
	}
	return values
}

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

func TestBuildBucketsRendersProvisionerValues(t *testing.T) {
	profile := minioProfile()
	c := profileConfig(map[string]clusterenv.StorageProfile{"s3": profile})
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "s3", Bucket: "media-prod-uploads", Provision: boolPointer(true),
	}}}

	got, err := BuildBuckets(app, "prod", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("buckets = %d, want 1", len(got))
	}
	b := got[0]
	if b.ReleaseName != "media-prod-uploads-bucket" || b.Namespace != "platform-storage" {
		t.Fatalf("isolated bucket release = %+v", b)
	}
	v := builtBucketValues(t, b)
	if v.Bucket != "media-prod-uploads" {
		t.Fatalf("bucket = %q", v.Bucket)
	}
	if v.Endpoint != profile.Endpoint || !v.PathStyle || v.Region != "" {
		t.Fatalf("endpoint wiring = %+v", v)
	}
	if v.Image != profile.Image {
		t.Fatalf("image = %q, want the profile's digest-pinned image", v.Image)
	}
	if v.Credentials.StoreRef.Name != "platform-local" || v.Credentials.StoreRef.Kind != "ClusterSecretStore" {
		t.Fatalf("credential store = %+v", v.Credentials.StoreRef)
	}
	if v.Credentials.AccessKeyID.Key != "example-object-storage" || v.Credentials.AccessKeyID.Property != "ACCESS_KEY_ID" {
		t.Fatalf("access key ref = %+v", v.Credentials.AccessKeyID)
	}
	if v.Credentials.SecretAccessKey.Key != "example-object-storage" || v.Credentials.SecretAccessKey.Property != "SECRET_ACCESS_KEY" {
		t.Fatalf("secret key ref = %+v", v.Credentials.SecretAccessKey)
	}

	// Renders are deterministic: the umbrella must not churn on a re-render.
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

func TestBuildBucketsCarriesRegionForVirtualHostProfiles(t *testing.T) {
	c := profileConfig(map[string]clusterenv.StorageProfile{"r2": r2Profile()})
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Provision: boolPointer(true),
	}}}
	got, err := BuildBuckets(app, "prod", c)
	if err != nil {
		t.Fatal(err)
	}
	values := builtBucketValues(t, got[0])
	if values.Region != "auto" || values.PathStyle {
		t.Fatalf("r2 addressing = region %q pathStyle %v, want auto/false", values.Region, values.PathStyle)
	}
}

func TestStorageWiringMatchesProvisionedBucket(t *testing.T) {
	c := profileConfig(map[string]clusterenv.StorageProfile{"s3": minioProfile()})
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "s3", Provision: boolPointer(true),
	}}}
	buckets, err := BuildBuckets(app, "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	env := buildAppEnv(app, "dev", c)
	values := builtBucketValues(t, buckets[0])
	if env["UPLOADS_BUCKET"] != values.Bucket {
		t.Fatalf("app bucket %q does not match provisioned bucket %q", env["UPLOADS_BUCKET"], values.Bucket)
	}
	if env["UPLOADS_ENDPOINT"] != minioProfile().Endpoint {
		t.Fatalf("endpoint = %q", env["UPLOADS_ENDPOINT"])
	}
	if env["UPLOADS_S3_PATH_STYLE"] != "true" {
		t.Fatalf("path style = %q, want %q", env["UPLOADS_S3_PATH_STYLE"], "true")
	}
	if _, present := env["UPLOADS_REGION"]; present {
		t.Fatalf("regionless profile emitted UPLOADS_REGION = %q", env["UPLOADS_REGION"])
	}
	if len(values.Bucket) > 63 {
		t.Fatalf("derived bucket name is not a DNS label: %q", values.Bucket)
	}

	// The regioned profile adds the region key and drops path-style.
	regioned := buildAppEnv(app, "dev", profileConfig(map[string]clusterenv.StorageProfile{"s3": r2Profile()}))
	if regioned["UPLOADS_REGION"] != "auto" {
		t.Fatalf("region = %q, want auto", regioned["UPLOADS_REGION"])
	}
	if _, present := regioned["UPLOADS_S3_PATH_STYLE"]; present {
		t.Fatalf("virtual-host profile emitted a path-style key")
	}
}

func TestExistingBucketWiringDoesNotProvision(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "r2", Bucket: "existing-bucket",
	}}}
	got, err := BuildBuckets(app, "prod", &clusterenv.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("default provision:false rendered %d bucket releases", len(got))
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
		t.Fatalf("missing storage profile error = %v", err)
	}
	c := profileConfig(map[string]clusterenv.StorageProfile{"r2": r2Profile()})
	app.Storage[0].Public = true
	if _, err := BuildBuckets(app, "prod", c); err == nil || !strings.Contains(err.Error(), "public bucket policy") {
		t.Fatalf("public provisioned bucket accepted: %v", err)
	}
}

func TestMultiComponentBucketProvisionedOnce(t *testing.T) {
	app := appconfig.App{
		App: "media",
		Storage: []appconfig.Storage{{
			Name: "uploads", Type: "s3", Provision: boolPointer(true),
		}},
		Components: []appconfig.Component{{Component: "api"}, {Component: "worker"}},
	}
	expanded := app.Expand()
	if !expanded[0].Storage[0].Provisioned() || expanded[1].Storage[0].Provisioned() {
		t.Fatalf("component ownership = first:%v second:%v",
			expanded[0].Storage[0].Provisioned(), expanded[1].Storage[0].Provisioned())
	}

	// Only the owning component renders a provisioner release, but BOTH get the
	// bucket's env wiring and its credential Secret — a sibling still reads it.
	c := profileConfig(map[string]clusterenv.StorageProfile{"s3": minioProfile()})
	owner, err := BuildBuckets(expanded[0], "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := BuildBuckets(expanded[1], "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 1 || len(sibling) != 0 {
		t.Fatalf("bucket releases = owner:%d sibling:%d, want 1 and 0", len(owner), len(sibling))
	}
	if got := buildStorage(expanded[1], c); len(got) != 1 || got[0].Prefix != "UPLOADS" {
		t.Fatalf("sibling storage credentials = %+v, want the shared bucket's key pair", got)
	}
}

func TestBuildStorageWiresPerBucketCredentialSecret(t *testing.T) {
	c := profileConfig(map[string]clusterenv.StorageProfile{"s3": minioProfile()})
	app := appconfig.App{
		App: "media", Component: "api",
		Storage: []appconfig.Storage{
			{Name: "uploads", Type: "s3", Provision: boolPointer(true)},
			// An unprofiled type keeps the legacy shared-credential wiring.
			{Name: "imports", Type: "r2", Bucket: "existing-imports"},
		},
	}
	got := buildStorage(app, c)
	if len(got) != 1 {
		t.Fatalf("storage credential entries = %d, want only the profile-backed bucket", len(got))
	}
	if got[0].Name != "uploads" || got[0].Prefix != "UPLOADS" {
		t.Fatalf("entry = %+v", got[0])
	}
	// Per WORKLOAD, so two components of one app never share a Secret name.
	if got[0].SecretName != "media-api-storage-uploads" {
		t.Fatalf("secret name = %q", got[0].SecretName)
	}
}

func TestProfileBackedCredentialsAreNotAlsoPinnedInTheRuntimeSecret(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{
		{Name: "uploads", Type: "r2", Provision: boolPointer(true)},
		{Name: "legacy", Type: "r2", Bucket: "existing-legacy"},
	}}

	// With NO profile, both buckets keep the legacy shared-credential path.
	if got := StorageSecretEnvKeys(app, nil); len(got) != 4 {
		t.Fatalf("unprofiled storage secret keys = %v, want all four", got)
	}
	// With a profile, the keys come from the per-bucket ExternalSecret instead,
	// so they must NOT also be claimed by the app runtime Secret.
	c := profileConfig(map[string]clusterenv.StorageProfile{"r2": r2Profile()})
	if got := StorageSecretEnvKeys(app, c); len(got) != 0 {
		t.Fatalf("profile-backed storage still pinned in the runtime Secret: %v", got)
	}
	for _, ref := range BuildExternalSecret(app, "prod", c).RemoteRefs {
		if strings.HasPrefix(ref.SecretKey, "UPLOADS_") || strings.HasPrefix(ref.SecretKey, "LEGACY_") {
			t.Fatalf("runtime Secret double-sources a profile-backed key: %+v", ref)
		}
	}
}

// TestRenderedValuesDiscloseNoCredential is the non-disclosure gate. A bucket's
// key pair must reach the pod ONLY through external-secrets, so every rendered
// surface that lands in Git — the umbrella entry and the app's plain env — may
// carry credential REFERENCES and never a place a value could sit.
func TestRenderedValuesDiscloseNoCredential(t *testing.T) {
	c := profileConfig(map[string]clusterenv.StorageProfile{"s3": minioProfile()})
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "s3", Provision: boolPointer(true),
	}}}

	values, err := BuildValues(app, "dev", c, "ghcr.io/example/media:v1", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	buckets, err := BuildBuckets(app, "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := yaml.Marshal(map[string]any{"values": values, "buckets": buckets})
	if err != nil {
		t.Fatal(err)
	}

	// The credential REFERENCES survive — losing them would silently unwire the
	// pod rather than leak anything, so assert they are present.
	for _, want := range []string{"example-object-storage", "platform-local", "ACCESS_KEY_ID"} {
		if !strings.Contains(string(rendered), want) {
			t.Fatalf("rendered output lost the credential reference %q:\n%s", want, rendered)
		}
	}

	// Non-secret bucket config is plain env; credential keys are NOT.
	extra := values.Env.Extra
	if extra["UPLOADS_BUCKET"] == "" || extra["UPLOADS_ENDPOINT"] == "" {
		t.Fatalf("non-secret bucket config missing from plain env: %+v", extra)
	}
	for key := range extra {
		if strings.HasSuffix(key, "_ACCESS_KEY_ID") || strings.HasSuffix(key, "_SECRET_ACCESS_KEY") {
			t.Fatalf("credential key %q rendered as plain env", key)
		}
	}

	// The dev backend writes a PLAIN Secret from devSecretPlaceholders. A
	// profile-backed bucket must be absent from it: its real key pair arrives
	// from the store, and a placeholder of the same name would either collide
	// with that Secret or quietly hand the app a non-working credential.
	for _, p := range values.DevSecretPlaceholders {
		if strings.HasPrefix(p.Name, "UPLOADS_") {
			t.Fatalf("profile-backed bucket got a dev placeholder credential: %+v", p)
		}
	}
	if len(values.Storage) != 1 || values.Storage[0].SecretName == "" {
		t.Fatalf("profile-backed bucket has no dedicated credential Secret: %+v", values.Storage)
	}
}

func TestToAppEntryCarriesBucketIntoIsolatedRelease(t *testing.T) {
	app := appconfig.App{App: "media", Storage: []appconfig.Storage{{
		Name: "uploads", Type: "s3", Provision: boolPointer(true),
	}}}
	buckets, err := BuildBuckets(app, "prod", profileConfig(
		map[string]clusterenv.StorageProfile{"s3": minioProfile()}))
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
	if entry.Buckets[0].ReleaseName != "media-prod-uploads-bucket" || entry.Buckets[0].Namespace != "platform-storage" {
		t.Fatalf("isolated bucket release = %+v", entry.Buckets[0])
	}
	if builtBucketValues(t, entry.Buckets[0]).Bucket != "media-prod-uploads" {
		t.Fatalf("bucket values lost: %+v", entry.Buckets[0].Values)
	}
	if len(entry.Buckets[0].Resource) != 0 {
		t.Fatalf("fresh render emitted a legacy managed resource: %+v", entry.Buckets[0].Resource)
	}
}
