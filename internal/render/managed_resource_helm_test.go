package render

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestManagedBucketChartsRenderIsolatedReleaseAndResource(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
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
      - namespace: crossplane-system
        releaseName: media-prod-uploads-bucket
        resource:
          apiVersion: r2.example.io/v1alpha1
          kind: Bucket
          metadata: {name: media-prod-uploads, namespace: crossplane-system}
          spec:
            managementPolicies: [Create, Observe, Update, LateInitialize]
            providerConfigRef: {name: default, kind: ProviderConfig}
            forProvider: {name: media-prod-uploads}
`
	valuesFile := writeHelmValues(t, clusterValues)
	out := helmTemplate(t, "platform-prod", "../../charts/cluster", valuesFile)
	for _, want := range []string{"kind: HelmRelease", "name: media-prod-uploads-bucket", "chart: ./charts/infra/managed-resource"} {
		if !strings.Contains(out, want) {
			t.Errorf("cluster output missing %q:\n%s", want, out)
		}
	}

	resourceValues := `
resource:
  apiVersion: r2.example.io/v1alpha1
  kind: Bucket
  metadata: {name: media-prod-uploads, namespace: crossplane-system}
  spec:
    managementPolicies: [Create, Observe, Update, LateInitialize]
    providerConfigRef: {name: default, kind: ProviderConfig}
    forProvider: {name: media-prod-uploads}
`
	out = helmTemplate(t, "media-prod-uploads-bucket", "../../charts/infra/managed-resource", writeHelmValues(t, resourceValues))
	for _, want := range []string{"kind: Bucket", "name: media-prod-uploads", "- Create", "- LateInitialize"} {
		if !strings.Contains(out, want) {
			t.Errorf("managed resource output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- Delete") {
		t.Fatalf("managed resource permits deletion:\n%s", out)
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
