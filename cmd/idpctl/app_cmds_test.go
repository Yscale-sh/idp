package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/compat/surrogate"
	"sigs.k8s.io/yaml"
)

var updateGolden = flag.Bool("update", false, "rewrite cmd/idpctl golden files")

const (
	carshowdbFile  = "../../examples/carshowdb/deploy.yaml"
	carshowdbImage = "registry.example.invalid/your-org/carshowdb-api:v1.4.0"
	mediaFile      = "../../examples/yscale-media/yscale-media.deploy.yaml"
	compatHeader   = "── rollback compatibility evidence ────────────\n"
)

// runPlan runs `idpctl plan` against an empty root (no environments/<env>), the
// same graceful no-env-config path a quick local check takes.
func runPlan(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newPlanCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--env", "dev", "--root", t.TempDir()}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("plan output differs from %s (run go test ./cmd/idpctl -update to inspect)\n--- got ---\n%s", path, got)
	}
}

// stripCompat removes every compatibility section, leaving what plan prints
// without --compat-evidence.
func stripCompat(s string) string {
	var b strings.Builder
	skipping := false
	for _, line := range strings.SplitAfter(s, "\n") {
		switch {
		case line == compatHeader:
			skipping = true
			continue
		case strings.HasPrefix(line, "── "):
			skipping = false
		}
		if !skipping {
			b.WriteString(line)
		}
	}
	return b.String()
}

// The goldens were captured from `idpctl plan` on main before --compat-evidence
// existed: plans without the flag must not change.
func TestPlan_WithoutEvidenceUnchanged(t *testing.T) {
	out, err := runPlan(t, "-f", carshowdbFile, "--image", carshowdbImage)
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "plan-carshowdb.golden.txt", out)

	out, err = runPlan(t, "-f", mediaFile, "--image", "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "plan-yscale-media.golden.txt", out)
}

func TestPlan_CompatEvidence(t *testing.T) {
	cases := []struct {
		name     string
		evidence string
		image    string
		want     []string
		reject   []string
	}{
		{
			name: "compatible", evidence: "compatible.yaml", image: carshowdbImage,
			want: []string{
				"result:     compatible (previous-version read probe passed on the migrated fixture; recorded by the producer; idpctl did not run it; limited: candidate matched by tag only)\n",
				"candidate:  " + carshowdbImage + " (tag only: a re-pushed tag makes this record stale; a tag does not identify image bytes)\n",
				"fixture:    synthetic/carshowdb-listings-v42\n",
				"probe:      go test ./compat -run TestPreviousImageReadsMigratedFixture (exit 0)\n",
				"  | --- PASS: TestPreviousImageReadsMigratedFixture (0.41s)\n",
			},
			reject: []string{"restore:", "unknown"},
		},
		{
			name: "incompatible", evidence: "incompatible.yaml", image: carshowdbImage,
			want: []string{
				"result:     incompatible (previous-version read probe failed; recorded by the producer; idpctl did not run it; git revert alone may not restore a working release)\n",
				"(exit 1)\n",
				`column "legacy_id" does not exist`,
				"restore:    restore the pre-0043 snapshot with the db-restore runbook, then git revert the release commit\n",
			},
		},
		{
			name: "unknown: probe not run", evidence: "unknown-not-run.yaml", image: carshowdbImage,
			want: []string{
				"result:     unknown (probe did not run (no exit code recorded)); this is not evidence that a revert is safe\n",
				"restore:    restore the pre-0043 snapshot",
			},
			reject: []string{"result:     compatible"},
		},
		{
			name: "unknown: evidence for another release", evidence: "compatible.yaml",
			image: "registry.example.invalid/your-org/carshowdb-api:v1.5.0",
			want:  []string{"result:     unknown (evidence was recorded for candidate", "restore:    none declared"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPlan(t, "-f", carshowdbFile, "--image", tc.image,
				"--compat-evidence", filepath.Join("testdata", "compat", tc.evidence))
			if err != nil {
				t.Fatalf("plan should report, not fail: %v", err)
			}
			if strings.Count(out, compatHeader) != 1 {
				t.Fatalf("want one compatibility section:\n%s", out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(out, r) {
					t.Errorf("unexpected %q in:\n%s", r, out)
				}
			}
			// The section sits between the summary and the rendered release.
			if strings.Index(out, compatHeader) > strings.Index(out, "── rendered Flux HelmRelease") {
				t.Errorf("compatibility section should precede the rendered release:\n%s", out)
			}
			if tc.image == carshowdbImage {
				assertGolden(t, "plan-carshowdb.golden.txt", stripCompat(out))
			}
		})
	}
}

func TestPlan_CompatEvidenceComponents(t *testing.T) {
	out, err := runPlan(t, "-f", mediaFile, "--image", "v1.4.0",
		"--compat-evidence", filepath.Join("testdata", "compat", "yscale-media.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	sections := strings.Split(out, compatHeader)[1:]
	if len(sections) != 4 {
		t.Fatalf("want a compatibility section per component (4), got %d:\n%s", len(sections), out)
	}
	want := []string{
		"result:     compatible",
		"result:     incompatible",
		"result:     unknown (no compatibility check recorded for yscale-media/transcode)",
		"result:     unknown (no compatibility check recorded for yscale-media/ui)",
	}
	for i, w := range want {
		if !strings.HasPrefix(sections[i], w) {
			t.Errorf("component %d: want %q, got:\n%s", i, w, sections[i])
		}
	}
	if !strings.Contains(sections[1], `  | scan_jobs.state: unknown enum value "deferred"`) {
		t.Errorf("scanner output excerpt missing:\n%s", sections[1])
	}
	assertGolden(t, "plan-yscale-media.golden.txt", stripCompat(out))
}

func TestPlan_MalformedEvidenceFails(t *testing.T) {
	for _, path := range []string{
		filepath.Join("testdata", "compat", "malformed.yaml"),
		filepath.Join("testdata", "compat", "does-not-exist.yaml"),
	} {
		out, err := runPlan(t, "-f", carshowdbFile, "--image", carshowdbImage, "--compat-evidence", path)
		if err == nil {
			t.Errorf("%s: plan should fail on unusable evidence", path)
		}
		if strings.Contains(out, "── plan") {
			t.Errorf("%s: plan printed output before rejecting the evidence:\n%s", path, out)
		}
	}
}

// TestPlan_SurrogateEvidence runs the synthetic surrogate (a Go stand-in for the
// previous reader, not an image), writes the evidence it derives from that run,
// and feeds it to the real plan command.
func TestPlan_SurrogateEvidence(t *testing.T) {
	const fixtures = "../../internal/compat/surrogate/testdata/"
	cases := []struct {
		name, fixture, migration string
		want                     []string
	}{
		{
			name: "additive", fixture: "listings-v41.json", migration: "0042_add_listings_vin.json",
			want: []string{
				"result:     compatible (previous-version read probe passed on the migrated fixture; synthetic surrogate reader, not the previous image; limited: candidate matched by tag only)\n",
				"reader:     synthetic surrogate (Go stand-in for the previous reader; no container image ran)\n",
				"fixture:    synthetic/listings-v41 (sha256:",
				"  | listings-reader@v41 after 0042_add_listings_vin: read 3 listings rows (columns id, legacy_id, title)\n",
			},
		},
		{
			name: "breaking", fixture: "listings-v41.json", migration: "0043_drop_listings_legacy_id.json",
			want: []string{
				"result:     incompatible (previous-version read probe failed; synthetic surrogate reader, not the previous image;",
				`  | listings-reader@v41 after 0043_drop_listings_legacy_id: column "legacy_id" does not exist`,
				"(exit 1)\n",
			},
		},
		{
			name: "missing fixture", fixture: "absent.json", migration: "0043_drop_listings_legacy_id.json",
			want: []string{"result:     unknown (probe did not run (no exit code recorded)); this is not evidence that a revert is safe\n", "fixture unusable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := yaml.Marshal(surrogate.Evidence(surrogate.Request{
				App: "carshowdb", Component: "api", Candidate: carshowdbImage,
				FixturePath: fixtures + tc.fixture, MigrationPath: fixtures + tc.migration,
			}))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "evidence.yaml")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runPlan(t, "-f", carshowdbFile, "--image", carshowdbImage, "--compat-evidence", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			if strings.Contains(out, "previous:") || strings.Contains(out, "external producer") {
				t.Errorf("surrogate evidence presented as an image run:\n%s", out)
			}
			assertGolden(t, "plan-carshowdb.golden.txt", stripCompat(out))
		})
	}
}
