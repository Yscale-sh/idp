package surrogate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/compat"
	"sigs.k8s.io/yaml"
)

const (
	base      = "testdata/listings-v41.json"
	additive  = "testdata/0042_add_listings_vin.json"
	breaking  = "testdata/0043_drop_listings_legacy_id.json"
	truncated = "testdata/listings-truncated.json"
	candidate = "registry.example.invalid/acme/carshowdb-api@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
)

// The previous reader must accept the unmigrated fixture, or every other
// result in this file is meaningless.
func TestReadPrevious_BaseFixture(t *testing.T) {
	var f Fixture
	if _, err := loadJSON(base, &f); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadPrevious(f)
	if err != nil {
		t.Fatalf("previous reader rejected its own schema: %v", err)
	}
	if msg != "read 3 listings rows (columns id, legacy_id, title)" {
		t.Errorf("unexpected reader output %q", msg)
	}
}

func TestProbe_AdditiveMigrationAccepted(t *testing.T) {
	o := Probe(base, additive)
	if !o.Ran || o.ExitCode != 0 {
		t.Fatalf("additive migration should be read by the previous reader: %+v", o)
	}
	if !strings.Contains(o.Output, "listings-reader@v41 after 0042_add_listings_vin: read 3 listings rows") {
		t.Errorf("output: %q", o.Output)
	}
}

// This is the check the surrogate exists for: if the breaking migration is ever
// accepted (a reader that stops selecting legacy_id, an Apply that no longer
// drops it), this test fails.
func TestProbe_BreakingMigrationRejected(t *testing.T) {
	o := Probe(base, breaking)
	if !o.Ran {
		t.Fatalf("breaking migration should run, not be unusable: %+v", o)
	}
	if o.ExitCode == 0 {
		t.Fatalf("previous reader ACCEPTED a migration that drops a column it reads: %+v", o)
	}
	if !strings.Contains(o.Output, `column "legacy_id" does not exist`) {
		t.Errorf("output should name the missing column: %q", o.Output)
	}
}

func TestProbe_UnusableInputsDoNotRun(t *testing.T) {
	dir := t.TempDir()
	unknownOp := filepath.Join(dir, "rename.json")
	if err := os.WriteFile(unknownOp, []byte(`{"migration":"9999_rename","ops":[{"op":"renameColumn","column":"title"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, paths := range map[string][2]string{
		"missing fixture":   {filepath.Join(dir, "absent.json"), breaking},
		"truncated fixture": {truncated, breaking},
		"missing migration": {base, filepath.Join(dir, "absent.json")},
		"unsupported op":    {base, unknownOp},
	} {
		o := Probe(paths[0], paths[1])
		if o.Ran {
			t.Errorf("%s: probe should not run: %+v", name, o)
		}
		if !strings.Contains(o.Output, "unusable") {
			t.Errorf("%s: output should say why: %q", name, o.Output)
		}
	}
}

func TestApply_DoesNotMutateInput(t *testing.T) {
	var f Fixture
	if _, err := loadJSON(base, &f); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(f, Migration{ID: "x", Ops: []Op{{Op: "dropColumn", Column: "legacy_id"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrevious(f); err != nil {
		t.Fatalf("Apply mutated the source fixture: %v", err)
	}
}

// evaluate round-trips surrogate evidence through YAML and compat.Parse, the
// same path `idpctl plan --compat-evidence` takes.
func evaluate(t *testing.T, req Request) (compat.Check, compat.Report) {
	t.Helper()
	raw, err := yaml.Marshal(Evidence(req))
	if err != nil {
		t.Fatal(err)
	}
	ev, err := compat.Parse(raw)
	if err != nil {
		t.Fatalf("surrogate evidence does not parse: %v\n%s", err, raw)
	}
	return ev.Checks[0], ev.Evaluate("carshowdb", "api", candidate)
}

func TestEvidence_OutcomeComesFromTheRun(t *testing.T) {
	req := Request{App: "carshowdb", Component: "api", Candidate: candidate, FixturePath: base}

	req.MigrationPath = additive
	c, r := evaluate(t, req)
	if r.Result != compat.Compatible || c.Reader != compat.ReaderSurrogate || c.PreviousImage != "" {
		t.Errorf("additive: want compatible surrogate record, got %s (%s) %+v", r.Result, r.Reason, c)
	}

	req.MigrationPath = breaking
	c, r = evaluate(t, req)
	if r.Result != compat.Incompatible || c.ExitCode == nil || *c.ExitCode == 0 {
		t.Errorf("breaking: want incompatible from a non-zero run, got %s %+v", r.Result, c)
	}

	req.FixturePath = filepath.Join(t.TempDir(), "absent.json")
	c, r = evaluate(t, req)
	if r.Result != compat.Unknown || c.ExitCode != nil || !strings.Contains(r.Reason, "did not run") {
		t.Errorf("missing fixture: want unknown with no exit code, got %s (%s) %+v", r.Result, r.Reason, c)
	}
}

// A record carries the digests of the exact fixture and migration it ran, so a
// record made from different inputs is visibly different.
func TestEvidence_IdentityExposesStaleRecords(t *testing.T) {
	req := Request{App: "carshowdb", Component: "api", Candidate: candidate, FixturePath: base, MigrationPath: additive}
	c1, _ := evaluate(t, req)
	if c1.FixtureDigest == "" || c1.MigrationDigest == "" || c1.Fixture != "synthetic/listings-v41" || c1.Migration != "0042_add_listings_vin" {
		t.Fatalf("record lacks fixture/migration identity: %+v", c1)
	}
	if !strings.Contains(c1.Probe, "--fixture "+base+" --migration "+additive) {
		t.Errorf("probe should be the re-runnable command: %q", c1.Probe)
	}

	edited := filepath.Join(t.TempDir(), "listings-v41.json")
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edited, []byte(strings.Replace(string(raw), "Synthetic coupe A", "Synthetic coupe Z", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	req.FixturePath = edited
	c2, _ := evaluate(t, req)
	if c2.FixtureDigest == c1.FixtureDigest {
		t.Error("an edited fixture kept the same digest")
	}
	if c2.MigrationDigest != c1.MigrationDigest {
		t.Error("the same migration produced a different digest")
	}
}
