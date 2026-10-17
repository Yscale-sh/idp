// Package compat reads rollback-compatibility evidence and classifies it for
// `idpctl plan --compat-evidence`.
//
// A git revert restores the previous image, not the data the newer release
// migrated. The question an operator needs answered before merging a migration
// is: "can the PREVIOUS version still read the data after this migration?" This
// package does not run that probe. A producer runs a previous-version reader
// against a fixture with the proposed migration applied and records the outcome
// in an evidence file. plan reads the file, checks it describes THIS release,
// and reports one of three results per app/component:
//
//   - compatible: the recorded probe ran and exited 0;
//   - incompatible: the recorded probe ran and exited non-zero;
//   - unknown: there is no usable record (missing check, probe not run, no
//     digest-pinned previous image, evidence for another release, no fixture or
//     probe reference). Unknown never implies that a revert is safe.
//
// Two producers are recognised, and plan names which one made the record:
//
//   - previous-image: an external job ran the previous container image. idpctl
//     cannot see that run; it trusts the recorded exit code.
//   - synthetic-surrogate: the in-repo surrogate (package compat/surrogate) ran a
//     Go stand-in for the previous reader over synthetic data. It demonstrates
//     the check; it is not the previous image and proves nothing about a real
//     database.
//
// One fixture is evidence about that fixture only; it does not prove arbitrary
// migrations safe and says nothing about writes made after the migration.
package compat

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// Kind and Version identify the evidence file format.
const (
	Kind    = "CompatibilityEvidence"
	Version = 1
)

// Result values.
const (
	Compatible   = "compatible"
	Incompatible = "incompatible"
	Unknown      = "unknown"
)

// Reader values: who ran the previous-version read probe.
const (
	ReaderPreviousImage = "previous-image"
	ReaderSurrogate     = "synthetic-surrogate"
)

// Output bounds: the plan shows at most this much of a probe's recorded output.
const (
	maxOutputLines    = 8
	maxOutputLineRune = 160
	maxFileBytes      = 1 << 20
)

// Evidence is one evidence file: the recorded read-probe checks for an app.
type Evidence struct {
	Kind    string  `json:"kind"`
	Version int     `json:"version"`
	App     string  `json:"app"`
	Checks  []Check `json:"checks"`
}

// Check is one previous-version read probe over one migrated fixture.
type Check struct {
	// Component matches the expanded component ("" for an app without one).
	Component string `json:"component,omitempty"`
	// Reader says what ran the probe: previous-image or synthetic-surrogate.
	Reader string `json:"reader"`
	// PreviousImage is the image a git revert would restore, pinned by digest
	// (repo@sha256:...). A tag can be re-pushed, so it does not identify what
	// the probe ran. Required for previous-image, rejected for the surrogate.
	PreviousImage string `json:"previousImage,omitempty"`
	// CandidateImage is the release that ships the migration. It must equal the
	// planned --image, so evidence for one release cannot vouch for another. A
	// tag match is weaker than a digest match: a re-pushed tag makes the record
	// stale without changing the string.
	CandidateImage string `json:"candidateImage"`
	// Migration names the migration applied to the fixture.
	Migration string `json:"migration,omitempty"`
	// MigrationDigest is the sha256 of the migration the probe applied.
	MigrationDigest string `json:"migrationDigest,omitempty"`
	// Fixture identifies the synthetic or public dataset the probe read.
	Fixture string `json:"fixture"`
	// FixtureDigest is the sha256 of the fixture before migration.
	FixtureDigest string `json:"fixtureDigest,omitempty"`
	// Probe is the command or test reference that was run, so a reviewer can
	// re-run it.
	Probe string `json:"probe"`
	// ExitCode is the probe's exit status. Absent means the probe did not run.
	ExitCode *int `json:"exitCode,omitempty"`
	// Output is the probe's recorded output; plan prints a bounded excerpt.
	Output string `json:"output,omitempty"`
	// RestorePath is the operator's stated way back when a revert alone is not
	// enough (e.g. "restore the pre-0042 snapshot per runbook db-restore").
	RestorePath string `json:"restorePath,omitempty"`
}

// Load reads and structurally validates an evidence file. A malformed file is
// an error rather than an unknown result: the operator asked plan to use it.
func Load(path string) (*Evidence, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read compatibility evidence: %w", err)
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("compatibility evidence %q is larger than %d bytes", path, maxFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read compatibility evidence: %w", err)
	}
	ev, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("compatibility evidence %q: %w", path, err)
	}
	return ev, nil
}

// Parse decodes evidence strictly (unknown fields and duplicate keys are
// rejected) and validates its structure.
func Parse(raw []byte) (*Evidence, error) {
	var ev Evidence
	if err := yaml.UnmarshalStrict(raw, &ev); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if ev.Kind != Kind {
		return nil, fmt.Errorf("kind must be %q, got %q", Kind, ev.Kind)
	}
	if ev.Version != Version {
		return nil, fmt.Errorf("version must be %d, got %d", Version, ev.Version)
	}
	if strings.TrimSpace(ev.App) == "" {
		return nil, fmt.Errorf("app is required")
	}
	seen := map[string]bool{}
	for i, c := range ev.Checks {
		if seen[c.Component] {
			return nil, fmt.Errorf("checks[%d]: duplicate check for component %q", i, c.Component)
		}
		seen[c.Component] = true
		switch c.Reader {
		case ReaderPreviousImage:
		case ReaderSurrogate:
			if c.PreviousImage != "" {
				return nil, fmt.Errorf("checks[%d]: previousImage does not apply to reader %q (no image ran)", i, ReaderSurrogate)
			}
		default:
			return nil, fmt.Errorf("checks[%d]: reader must be %q or %q, got %q", i, ReaderPreviousImage, ReaderSurrogate, c.Reader)
		}
		for field, d := range map[string]string{"fixtureDigest": c.FixtureDigest, "migrationDigest": c.MigrationDigest} {
			if d != "" && !isDigest(d) {
				return nil, fmt.Errorf("checks[%d]: %s must be sha256:<64 hex>, got %q", i, field, d)
			}
		}
	}
	return &ev, nil
}

// Report is the classified result for one planned app/component.
type Report struct {
	Result string
	Reason string // why the result is unknown; empty otherwise
	Check  *Check // the matching check, nil when none was recorded
}

// Evaluate classifies the evidence for one planned app/component and image.
// It never returns compatible without a recorded, passing probe that names its
// reader (a digest-pinned previous image, or the surrogate with fixture and
// migration digests), this candidate image, a fixture, and a probe.
func (e *Evidence) Evaluate(app, component, image string) Report {
	target := app
	if component != "" {
		target += "/" + component
	}
	if e.App != app {
		return Report{Result: Unknown, Reason: fmt.Sprintf("evidence is for app %q, not %q", e.App, app)}
	}
	var c *Check
	for i := range e.Checks {
		if e.Checks[i].Component == component {
			c = &e.Checks[i]
			break
		}
	}
	if c == nil {
		return Report{Result: Unknown, Reason: "no compatibility check recorded for " + target}
	}
	unknown := func(reason string) Report { return Report{Result: Unknown, Reason: reason, Check: c} }
	switch {
	case c.Reader == ReaderPreviousImage && c.PreviousImage == "":
		return unknown("no previous image recorded")
	case c.Reader == ReaderPreviousImage && !pinned(c.PreviousImage):
		return unknown(fmt.Sprintf("previous image %q is not pinned by digest (repo@sha256:...)", c.PreviousImage))
	case image == "":
		return unknown("plan has no --image, so the evidence cannot be matched to this release")
	case c.CandidateImage != image:
		return unknown(fmt.Sprintf("evidence was recorded for candidate %q, not the planned image %q", c.CandidateImage, image))
	case strings.TrimSpace(c.Fixture) == "":
		return unknown("no fixture recorded")
	case strings.TrimSpace(c.Probe) == "":
		return unknown("no probe command or test reference recorded")
	case c.ExitCode == nil:
		return unknown("probe did not run (no exit code recorded)")
	case c.Reader == ReaderSurrogate && (c.FixtureDigest == "" || c.MigrationDigest == ""):
		return unknown("surrogate record has no fixture or migration digest, so a stale record cannot be detected")
	case *c.ExitCode == 0:
		return Report{Result: Compatible, Check: c}
	default:
		return Report{Result: Incompatible, Check: c}
	}
}

// pinned reports whether ref is pinned by a sha256 digest. Tags, including
// version tags, are mutable and do not count.
func pinned(ref string) bool {
	repo, digest, ok := strings.Cut(ref, "@")
	return ok && repo != "" && isDigest(digest)
}

// isDigest reports whether d is sha256:<64 lowercase hex>.
func isDigest(d string) bool {
	hex, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	for _, r := range hex {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// Summary renders the report as plan lines, aligned with deploy.Plan.Summary.
func (r Report) Summary() string {
	var b strings.Builder
	c := r.Check
	surrogate := c != nil && c.Reader == ReaderSurrogate
	var who string
	switch {
	case surrogate:
		who = "synthetic surrogate reader, not the previous image"
	default:
		who = "recorded by the producer; idpctl did not run it"
	}
	switch r.Result {
	case Compatible:
		qual := who
		if !pinned(c.CandidateImage) {
			qual += "; limited: candidate matched by tag only"
		}
		fmt.Fprintf(&b, "result:     compatible (previous-version read probe passed on the migrated fixture; %s)\n", qual)
	case Incompatible:
		fmt.Fprintf(&b, "result:     incompatible (previous-version read probe failed; %s; git revert alone may not restore a working release)\n", who)
	default:
		fmt.Fprintf(&b, "result:     unknown (%s); this is not evidence that a revert is safe\n", oneLine(r.Reason))
	}
	if c != nil {
		line := func(label, v string) {
			if v != "" {
				fmt.Fprintf(&b, "%-12s%s\n", label+":", oneLine(v))
			}
		}
		withDigest := func(v, d string) string {
			if v != "" && d != "" {
				return v + " (" + d + ")"
			}
			return v
		}
		if surrogate {
			line("reader", "synthetic surrogate (Go stand-in for the previous reader; no container image ran)")
		} else {
			line("reader", "previous image (external producer)")
			line("previous", c.PreviousImage)
		}
		if c.CandidateImage != "" {
			binding := "digest-pinned"
			if !pinned(c.CandidateImage) {
				binding = "tag only: a re-pushed tag makes this record stale; a tag does not identify image bytes"
			}
			line("candidate", c.CandidateImage+" ("+binding+")")
		}
		line("fixture", withDigest(c.Fixture, c.FixtureDigest))
		line("migration", withDigest(c.Migration, c.MigrationDigest))
		if c.Probe != "" {
			probe := oneLine(c.Probe)
			if c.ExitCode != nil {
				probe += fmt.Sprintf(" (exit %d)", *c.ExitCode)
			}
			line("probe", probe)
		}
		if out := excerpt(c.Output); out != "" {
			b.WriteString("output:\n")
			b.WriteString(out)
		}
	}
	if r.Result != Compatible {
		restore := "none declared; decide one before merging this migration"
		if c != nil && strings.TrimSpace(c.RestorePath) != "" {
			restore = c.RestorePath
		}
		fmt.Fprintf(&b, "restore:    %s\n", oneLine(restore))
	}
	b.WriteString("scope:      one fixture read probe; not proof for other data, and writes after the migration are not covered\n")
	return b.String()
}

// excerpt returns at most maxOutputLines indented lines of out, each at most
// maxOutputLineRune runes, with control characters removed.
func excerpt(out string) string {
	out = strings.TrimRight(out, "\n")
	if strings.TrimSpace(out) == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	var b strings.Builder
	for i, l := range lines {
		if i == maxOutputLines {
			fmt.Fprintf(&b, "  | … %d more line(s) not shown\n", len(lines)-maxOutputLines)
			break
		}
		l = clean(l)
		if utf8.RuneCountInString(l) > maxOutputLineRune {
			l = string([]rune(l)[:maxOutputLineRune]) + "…"
		}
		fmt.Fprintf(&b, "  | %s\n", l)
	}
	return b.String()
}

// oneLine collapses a value to one clean line so evidence cannot forge extra
// plan lines.
func oneLine(s string) string {
	return strings.Join(strings.Fields(clean(s)), " ")
}

// clean drops control characters (including the ESC byte that starts a
// terminal escape sequence), turning tabs and line breaks into spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t', r == '\n', r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}
