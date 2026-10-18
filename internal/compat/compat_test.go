package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	prev = "registry.example.invalid/acme/api@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cand = "registry.example.invalid/acme/api:v2"
)

func intPtr(i int) *int { return &i }

// passing returns a complete check that classifies as compatible.
func passing() Check {
	return Check{
		Component: "api", Reader: ReaderPreviousImage, PreviousImage: prev, CandidateImage: cand,
		Fixture: "synthetic/orders-v7", Probe: "go test ./compat", ExitCode: intPtr(0),
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name   string
		app    string
		image  string
		mutate func(*Check)
		want   string
		reason string
	}{
		{name: "passing probe", want: Compatible},
		{name: "failing probe", mutate: func(c *Check) { c.ExitCode = intPtr(1) }, want: Incompatible},
		{name: "probe did not run", mutate: func(c *Check) { c.ExitCode = nil }, want: Unknown, reason: "did not run"},
		{name: "no check for component", mutate: func(c *Check) { c.Component = "worker" }, want: Unknown, reason: "no compatibility check recorded for acme/api"},
		{name: "evidence for another app", app: "other", want: Unknown, reason: `evidence is for app "acme"`},
		{name: "no previous image", mutate: func(c *Check) { c.PreviousImage = "" }, want: Unknown, reason: "no previous image"},
		{name: "previous image latest", mutate: func(c *Check) { c.PreviousImage = "registry.example.invalid/acme/api:latest" }, want: Unknown, reason: "not pinned"},
		{name: "previous image version tag", mutate: func(c *Check) { c.PreviousImage = "registry.example.invalid/acme/api:v1.2.3" }, want: Unknown, reason: "not pinned by digest"},
		{name: "previous image tagless", mutate: func(c *Check) { c.PreviousImage = "registry.example.invalid:5000/acme/api" }, want: Unknown, reason: "not pinned"},
		{name: "plan without image", image: "-", want: Unknown, reason: "no --image"},
		{name: "evidence for another candidate", mutate: func(c *Check) { c.CandidateImage = "registry.example.invalid/acme/api:v1" }, want: Unknown, reason: "not the planned image"},
		{name: "no candidate recorded", mutate: func(c *Check) { c.CandidateImage = "" }, want: Unknown, reason: "not the planned image"},
		{name: "no fixture", mutate: func(c *Check) { c.Fixture = " " }, want: Unknown, reason: "no fixture"},
		{name: "no probe reference", mutate: func(c *Check) { c.Probe = "" }, want: Unknown, reason: "no probe"},
		{name: "digest-pinned candidate", mutate: func(c *Check) { c.CandidateImage = prev }, image: prev, want: Compatible},
		{name: "surrogate with digests", mutate: surrogateCheck, want: Compatible},
		{name: "surrogate failing", mutate: func(c *Check) { surrogateCheck(c); c.ExitCode = intPtr(1) }, want: Incompatible},
		{name: "surrogate not run", mutate: func(c *Check) { surrogateCheck(c); c.ExitCode = nil; c.FixtureDigest = "" }, want: Unknown, reason: "did not run"},
		{name: "surrogate without digests", mutate: func(c *Check) { surrogateCheck(c); c.MigrationDigest = "" }, want: Unknown, reason: "no fixture or migration digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := passing()
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			ev := &Evidence{Kind: Kind, Version: Version, App: "acme", Checks: []Check{c}}
			app := "acme"
			if tc.app != "" {
				app = tc.app
			}
			image := cand
			switch tc.image {
			case "":
			case "-":
				image = ""
			default:
				image = tc.image
			}
			got := ev.Evaluate(app, "api", image)
			if got.Result != tc.want {
				t.Fatalf("result = %q (%s), want %q", got.Result, got.Reason, tc.want)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", got.Reason, tc.reason)
			}
		})
	}
}

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// surrogateCheck turns a passing previous-image check into a passing surrogate one.
func surrogateCheck(c *Check) {
	c.Reader, c.PreviousImage = ReaderSurrogate, ""
	c.FixtureDigest, c.MigrationDigest = digestA, digestA
}

func TestEvaluate_SingleComponentApp(t *testing.T) {
	c := passing()
	c.Component = ""
	ev := &Evidence{Kind: Kind, Version: Version, App: "acme", Checks: []Check{c}}
	if got := ev.Evaluate("acme", "", cand); got.Result != Compatible {
		t.Fatalf("app without component: got %+v", got)
	}
	if got := ev.Evaluate("acme", "api", cand); got.Result != Unknown {
		t.Fatalf("an app-level check must not cover a named component: got %+v", got)
	}
}

func TestPinned(t *testing.T) {
	for ref, want := range map[string]bool{
		prev: true,
		"ghcr.io/acme/api:v1.2.3@sha256:" + strings.Repeat("a", 64): true,
		"ghcr.io/acme/api:v1.2.3":                                   false,
		"ghcr.io/acme/api:latest":                                   false,
		"ghcr.io/acme/api":                                          false,
		"localhost:5000/acme/api:2026-10":                           false,
		"ghcr.io/acme/api@sha256:abc":                               false,
		"ghcr.io/acme/api@sha256:" + strings.Repeat("Z", 64):        false,
		"@sha256:" + strings.Repeat("a", 64):                        false,
		" @" + digestA:                                              false,
		"https://ghcr.io/acme/api@" + digestA:                       false,
		"ghcr.io/acme/../api@" + digestA:                            false,
		"ghcr.io/acme/api\n@" + digestA:                             false,
	} {
		if got := pinned(ref); got != want {
			t.Errorf("pinned(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestParseRejectsTrailingDocuments(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/idpctl/testdata/compat/compatible.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"\n---\n" + string(raw), "\n---\nmalformed: [", "\n---\n"} {
		if _, err := Parse(append(append([]byte(nil), raw...), suffix...)); err == nil {
			t.Errorf("accepted trailing document %q", suffix)
		}
	}
	if _, err := Parse(append(raw, []byte("\n# trailing comment\n")...)); err != nil {
		t.Fatalf("rejected trailing whitespace/comment: %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat(" ", maxFileBytes+1))); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Parse must enforce the byte limit: %v", err)
	}
}

func TestSummaryRemovesDirectionalControls(t *testing.T) {
	c := passing()
	c.Output = "failed\u202epassed\u2066hidden\u2069"
	c.RestorePath = "restore\u202e" + strings.Repeat("x", 1000)
	s := (Report{Result: Incompatible, Check: &c}).Summary()
	if strings.ContainsAny(s, "\u202e\u2066\u2069") || strings.Contains(s, strings.Repeat("x", 1000)) {
		t.Fatal("untrusted metadata can spoof or flood terminal output")
	}
}

func TestParse_Malformed(t *testing.T) {
	cases := map[string]string{
		"not yaml":          "kind: [",
		"unknown field":     "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - dataCompatible: true\n",
		"duplicate key":     "kind: CompatibilityEvidence\nversion: 1\napp: acme\napp: other\n",
		"wrong kind":        "kind: Evidence\nversion: 1\napp: acme\n",
		"missing kind":      "version: 1\napp: acme\n",
		"wrong version":     "kind: CompatibilityEvidence\nversion: 2\napp: acme\n",
		"missing app":       "kind: CompatibilityEvidence\nversion: 1\n",
		"string exit code":  "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - exitCode: passed\n",
		"duplicate checks":  "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - component: api\n  - component: api\n",
		"duplicate default": "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - reader: previous-image\n  - reader: previous-image\n",
		"missing reader":    "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - probe: a\n",
		"unknown reader":    "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - reader: ci\n",
		"surrogate image":   "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - reader: synthetic-surrogate\n    previousImage: x@" + digestA + "\n",
		"bad digest":        "kind: CompatibilityEvidence\nversion: 1\napp: acme\nchecks:\n  - reader: synthetic-surrogate\n    fixtureDigest: sha256:abc\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected a parse error", name)
		}
	}
	if _, err := Parse([]byte("kind: CompatibilityEvidence\nversion: 1\napp: acme\n")); err != nil {
		t.Errorf("minimal evidence rejected: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("missing file should error")
	}
	big := filepath.Join(dir, "big.yaml")
	if err := os.WriteFile(big, make([]byte, maxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized file should error, got %v", err)
	}
}

func TestSummary(t *testing.T) {
	c := passing()
	ok := Report{Result: Compatible, Check: &c}.Summary()
	for _, want := range []string{"result:     compatible", "previous:   " + prev, "fixture:    synthetic/orders-v7", "probe:      go test ./compat (exit 0)", "scope:"} {
		if !strings.Contains(ok, want) {
			t.Errorf("compatible summary missing %q:\n%s", want, ok)
		}
	}
	if strings.Contains(ok, "restore:") {
		t.Errorf("compatible summary should not print a restore path:\n%s", ok)
	}
	for _, want := range []string{
		"recorded by the producer; idpctl did not run it; limited: candidate matched by tag only)",
		"candidate:  " + cand + " (tag only: a re-pushed tag makes this record stale; a tag does not identify image bytes)",
		"reader:     previous image (external producer)",
	} {
		if !strings.Contains(ok, want) {
			t.Errorf("tag-matched compatible summary missing %q:\n%s", want, ok)
		}
	}
	pinnedCand := c
	pinnedCand.CandidateImage = prev
	if s := (Report{Result: Compatible, Check: &pinnedCand}).Summary(); strings.Contains(s, "limited") || !strings.Contains(s, "(digest-pinned)") {
		t.Errorf("digest-pinned candidate should not be labelled limited:\n%s", s)
	}
	sur := c
	surrogateCheck(&sur)
	s0 := Report{Result: Compatible, Check: &sur}.Summary()
	for _, want := range []string{"synthetic surrogate reader, not the previous image", "reader:     synthetic surrogate (Go stand-in for the previous reader; no container image ran)", "fixture:    synthetic/orders-v7 (" + digestA + ")"} {
		if !strings.Contains(s0, want) {
			t.Errorf("surrogate summary missing %q:\n%s", want, s0)
		}
	}
	if strings.Contains(s0, "previous:") || strings.Contains(s0, "external producer") {
		t.Errorf("surrogate summary must not present an image run:\n%s", s0)
	}

	bad := c
	bad.ExitCode = intPtr(3)
	bad.RestorePath = "restore snapshot pre-0042"
	s := Report{Result: Incompatible, Check: &bad}.Summary()
	for _, want := range []string{"result:     incompatible", "(exit 3)", "restore:    restore snapshot pre-0042"} {
		if !strings.Contains(s, want) {
			t.Errorf("incompatible summary missing %q:\n%s", want, s)
		}
	}

	s = Report{Result: Unknown, Reason: "no compatibility check recorded for acme/api"}.Summary()
	for _, want := range []string{"result:     unknown (no compatibility check recorded for acme/api); this is not evidence that a revert is safe", "restore:    none declared"} {
		if !strings.Contains(s, want) {
			t.Errorf("unknown summary missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "compatible (") {
		t.Errorf("unknown summary must not read as compatible:\n%s", s)
	}
}

func TestSummary_BoundsAndSanitizesOutput(t *testing.T) {
	c := passing()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line")
	}
	lines[0] = "\x1b[31mred\x1b[0m"
	lines[1] = strings.Repeat("x", 500)
	c.Output = strings.Join(lines, "\n")
	c.Fixture = "fixture\nresult:     compatible" // a value must not forge a plan line
	s := Report{Result: Incompatible, Check: &c}.Summary()
	if strings.Contains(s, "\x1b") {
		t.Errorf("escape byte leaked into the plan:\n%q", s)
	}
	if n := strings.Count(s, "  | "); n != maxOutputLines+1 {
		t.Errorf("want %d excerpt lines plus a truncation marker, got %d:\n%s", maxOutputLines, n, s)
	}
	if !strings.Contains(s, "12 more line(s) not shown") {
		t.Errorf("missing truncation marker:\n%s", s)
	}
	if !strings.Contains(s, strings.Repeat("x", maxOutputLineRune)+"…\n") || strings.Contains(s, strings.Repeat("x", maxOutputLineRune+1)) {
		t.Errorf("long line not truncated to %d runes:\n%s", maxOutputLineRune, s)
	}
	if strings.Count(s, "\nresult:") != 0 || !strings.Contains(s, "fixture:    fixture result: compatible\n") {
		t.Errorf("a multi-line value forged an extra plan line:\n%s", s)
	}
}
