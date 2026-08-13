package render

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// bucketChartValues is the values the renderer produces for one provisioned
// bucket. PLACEHOLDERS ONLY — no real endpoint or credential path.
const bucketChartValues = `
bucket: media-prod-uploads
endpoint: https://minio.example.invalid
pathStyle: true
image: minio/mc@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
credentials:
  storeRef: {name: platform-local, kind: ClusterSecretStore}
  refreshInterval: 1h
  accessKeyID: {key: example-object-storage, property: ACCESS_KEY_ID}
  secretAccessKey: {key: example-object-storage, property: SECRET_ACCESS_KEY}
labels:
  platform/app: media
  platform/env: prod
`

func TestClusterChartRendersIsolatedBucketRelease(t *testing.T) {
	requireHelm(t)
	clusterValues := `
env: prod
source: {name: flux-system, namespace: flux-system}
modules: []
apps:
  - name: media
    namespace: media-prod
    releaseName: media
    values: {}
    buckets:
      - namespace: platform-storage
        releaseName: media-prod-uploads-bucket
        values:
` + indentBlock(bucketChartValues, "          ")

	out := helmTemplate(t, "platform-prod", "../../charts/cluster", writeHelmValues(t, clusterValues))
	for _, want := range []string{
		"kind: HelmRelease",
		"name: media-prod-uploads-bucket",
		"chart: ./charts/infra/bucket-provisioner",
		"targetNamespace: platform-storage",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster output missing %q:\n%s", want, out)
		}
	}
	// Bucket releases keep the NORMAL Helm wait. disableWait would let the
	// release report Ready before the provisioning hook finished, which is the
	// one thing waiting on a bucket is for.
	bucketRelease := releaseBlock(t, out, "media-prod-uploads-bucket")
	if strings.Contains(bucketRelease, "disableWait") {
		t.Errorf("bucket release disables the readiness wait:\n%s", bucketRelease)
	}
	if !strings.Contains(bucketRelease, "retries: 3") {
		t.Errorf("bucket release lost its remediation retries:\n%s", bucketRelease)
	}
}

func TestBucketProvisionerJobIsAWaitedHookThatNeverDeletes(t *testing.T) {
	requireHelm(t)
	out := helmTemplate(t, "media-prod-uploads-bucket", "../../charts/infra/bucket-provisioner",
		writeHelmValues(t, bucketChartValues))

	// A post-install/post-upgrade hook is what makes HelmRelease readiness wait
	// for the bucket to exist.
	for _, want := range []string{
		"helm.sh/hook: post-install,post-upgrade",
		"helm.sh/hook-delete-policy: before-hook-creation",
		"ttlSecondsAfterFinished:",
		"mb --ignore-existing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("provisioner output missing %q:\n%s", want, out)
		}
	}
	// hook-succeeded would delete the Job the instant it passed, defeating the
	// TTL and leaving nothing to inspect.
	if strings.Contains(out, "hook-succeeded") {
		t.Errorf("completed hook Job is deleted immediately instead of aging out:\n%s", out)
	}
	// There is no delete path, by design: tearing down an app must never
	// destroy its data.
	for _, forbidden := range []string{"mc rb", "--force", "mc rm"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("provisioner can destroy data (%q):\n%s", forbidden, out)
		}
	}
	// Credentials arrive by reference only.
	if !strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("no ExternalSecret rendered for the key pair:\n%s", out)
	}
	if !strings.Contains(out, "secretRef:") || !strings.Contains(out, "media-prod-uploads-bucket-creds") {
		t.Errorf("Job does not envFrom the credential Secret:\n%s", out)
	}
	// Path-style addressing reaches mc.
	if !strings.Contains(out, "--path on") {
		t.Errorf("pathStyle:true did not reach the mc alias:\n%s", out)
	}
}

// TestBucketProvisionerJobIdentityIncludesCredentialRefs pins the immutable-Job
// rule: a Job's pod template cannot be edited in place, so anything that changes
// what the Job does must produce a differently-named Job. Rotating the profile
// onto a different key pair counts — the new pair may not own the bucket yet, so
// provisioning has to re-run.
func TestBucketProvisionerJobIdentityIncludesCredentialRefs(t *testing.T) {
	requireHelm(t)
	base := jobName(t, bucketChartValues)

	for _, tc := range []struct {
		name    string
		values  string
		wantNew bool
	}{
		{"same input", bucketChartValues, false},
		{"rotated access key ref", strings.Replace(bucketChartValues,
			"accessKeyID: {key: example-object-storage, property: ACCESS_KEY_ID}",
			"accessKeyID: {key: example-object-storage-v2, property: ACCESS_KEY_ID}", 1), true},
		{"rotated secret key ref", strings.Replace(bucketChartValues,
			"secretAccessKey: {key: example-object-storage, property: SECRET_ACCESS_KEY}",
			"secretAccessKey: {key: example-object-storage-v2, property: SECRET_ACCESS_KEY}", 1), true},
		{"different store", strings.Replace(bucketChartValues,
			"name: platform-local", "name: platform-ssm", 1), true},
		{"different endpoint", strings.Replace(bucketChartValues,
			"https://minio.example.invalid", "https://other.example.invalid", 1), true},
		{"different bucket", strings.Replace(bucketChartValues,
			"media-prod-uploads", "media-dev-uploads", 1), true},
		// A label change is cosmetic and must NOT churn the Job.
		{"different labels", strings.Replace(bucketChartValues,
			"platform/env: prod", "platform/env: staging", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := jobName(t, tc.values)
			if tc.wantNew && got == base {
				t.Fatalf("changing %s reused Job name %q — the immutable Job would never re-run", tc.name, got)
			}
			if !tc.wantNew && got != base {
				t.Fatalf("%s changed the Job name (%q -> %q) and would re-run provisioning for nothing", tc.name, base, got)
			}
		})
	}
}

var jobNameRE = regexp.MustCompile(`(?m)^  name: (media-\S+)$`)

// jobName renders the chart and returns the Job's metadata.name.
func jobName(t *testing.T, values string) string {
	t.Helper()
	out := helmTemplate(t, "media-prod-uploads-bucket", "../../charts/infra/bucket-provisioner",
		writeHelmValues(t, values))
	for _, doc := range strings.Split(out, "\n---\n") {
		if !strings.Contains(doc, "kind: Job") {
			continue
		}
		m := jobNameRE.FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("could not read the Job name from:\n%s", doc)
		}
		return m[1]
	}
	t.Fatalf("no Job in rendered output:\n%s", out)
	return ""
}

// releaseBlock returns the rendered document declaring the named HelmRelease.
func releaseBlock(t *testing.T, out, name string) string {
	t.Helper()
	for _, doc := range strings.Split(out, "\n---\n") {
		if strings.Contains(doc, "name: "+name+"\n") {
			return doc
		}
	}
	t.Fatalf("no release %q in:\n%s", name, out)
	return ""
}

// indentBlock re-indents a YAML block so it can be nested under a key.
func indentBlock(block, indent string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.Trim(block, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(indent + line + "\n")
	}
	return b.String()
}

func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
}

func writeHelmValues(t *testing.T, values string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(values); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func helmTemplate(t *testing.T, release, chart, valuesFile string) string {
	t.Helper()
	cmd := exec.Command("helm", "template", release, chart, "-f", valuesFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template %s failed: %v\nstderr: %s", chart, err, stderr.String())
	}
	return stdout.String()
}
