// Package surrogate is a runnable, synthetic stand-in for the rollback
// compatibility probe that `idpctl plan --compat-evidence` reports.
//
// The real probe runs the PREVIOUS container image against data with the new
// migration applied. This package shows that check end to end without an image
// or a database: a small JSON table snapshot stands in for the data, a list of
// column operations stands in for the migration, and ReadPrevious stands in for
// the previous release's reader (it selects the columns that version needs and
// ignores the rest). Probe applies the migration and runs the reader, and
// Evidence turns that run into a compat.Check whose exit code comes from the run.
//
// It is NOT the previous image and proves nothing about any real database. It
// is opt-in tooling and test code; idpctl does not import it, and plan never
// runs it.
package surrogate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/yscale-sh/idp/internal/compat"
)

// ReaderID names the surrogate reader so a record shows which reader ran.
const ReaderID = "listings-reader@v41"

// Column is one column of a fixture table.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // int | text
}

// Fixture is a synthetic single-table snapshot.
type Fixture struct {
	ID      string   `json:"fixture"`
	Note    string   `json:"note,omitempty"`
	Table   string   `json:"table"`
	Columns []Column `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// Op is one migration step.
type Op struct {
	Op     string `json:"op"` // addColumn | dropColumn
	Column string `json:"column"`
	Type   string `json:"type,omitempty"`
}

// Migration is an ordered list of column operations.
type Migration struct {
	ID   string `json:"migration"`
	Note string `json:"note,omitempty"`
	Ops  []Op   `json:"ops"`
}

// Apply returns f with m applied. addColumn appends a nullable column; an
// unusable migration (unknown op, duplicate or missing column) is an error.
func Apply(f Fixture, m Migration) (Fixture, error) {
	out := Fixture{ID: f.ID, Table: f.Table, Columns: append([]Column(nil), f.Columns...)}
	out.Rows = make([][]any, len(f.Rows))
	for i, r := range f.Rows {
		out.Rows[i] = append([]any(nil), r...)
	}
	for _, op := range m.Ops {
		idx := columnIndex(out.Columns, op.Column)
		switch op.Op {
		case "addColumn":
			if idx >= 0 {
				return Fixture{}, fmt.Errorf("addColumn %q: column already exists", op.Column)
			}
			if op.Type != "int" && op.Type != "text" {
				return Fixture{}, fmt.Errorf("addColumn %q: type must be int or text", op.Column)
			}
			out.Columns = append(out.Columns, Column{Name: op.Column, Type: op.Type})
			for i := range out.Rows {
				out.Rows[i] = append(out.Rows[i], nil)
			}
		case "dropColumn":
			if idx < 0 {
				return Fixture{}, fmt.Errorf("dropColumn %q: no such column", op.Column)
			}
			out.Columns = append(out.Columns[:idx], out.Columns[idx+1:]...)
			for i, r := range out.Rows {
				out.Rows[i] = append(r[:idx], r[idx+1:]...)
			}
		default:
			return Fixture{}, fmt.Errorf("unknown op %q", op.Op)
		}
	}
	return out, nil
}

// previousColumns is what the v41 reader selects from listings.
var previousColumns = []Column{{"id", "int"}, {"legacy_id", "int"}, {"title", "text"}}

// ReadPrevious is the surrogate previous reader. Like a SELECT of named
// columns, it ignores columns it does not use and fails on a missing column, a
// changed type, or a NULL in a column it requires.
func ReadPrevious(f Fixture) (string, error) {
	if f.Table != "listings" {
		return "", fmt.Errorf("relation %q does not exist", "listings")
	}
	idx := make([]int, len(previousColumns))
	for i, want := range previousColumns {
		j := columnIndex(f.Columns, want.Name)
		if j < 0 {
			return "", fmt.Errorf("column %q does not exist", want.Name)
		}
		if f.Columns[j].Type != want.Type {
			return "", fmt.Errorf("column %q is %s, reader expects %s", want.Name, f.Columns[j].Type, want.Type)
		}
		idx[i] = j
	}
	for n, row := range f.Rows {
		if len(row) != len(f.Columns) {
			return "", fmt.Errorf("row %d has %d values for %d columns", n, len(row), len(f.Columns))
		}
		for i, want := range previousColumns {
			if !ofType(row[idx[i]], want.Type) {
				return "", fmt.Errorf("row %d: %s = %v is not a non-null %s", n, want.Name, row[idx[i]], want.Type)
			}
		}
	}
	names := make([]string, len(previousColumns))
	for i, c := range previousColumns {
		names[i] = c.Name
	}
	return fmt.Sprintf("read %d %s rows (columns %s)", len(f.Rows), f.Table, strings.Join(names, ", ")), nil
}

// Outcome is one surrogate probe run.
type Outcome struct {
	Ran             bool // false when the fixture or migration was unusable
	ExitCode        int  // 0 accepted, 1 rejected; meaningful only when Ran
	Output          string
	FixtureID       string
	FixtureDigest   string
	MigrationID     string
	MigrationDigest string
}

// Probe loads the fixture and migration, applies the migration, and runs the
// previous reader. A missing or unusable fixture or migration leaves Ran false,
// so the record classifies as unknown rather than as a pass or a fail.
func Probe(fixturePath, migrationPath string) Outcome {
	var o Outcome
	var f Fixture
	var m Migration
	var err error
	if o.FixtureDigest, err = loadJSON(fixturePath, &f); err != nil {
		o.Output = "fixture unusable: " + err.Error()
		return o
	}
	o.FixtureID = f.ID
	if o.MigrationDigest, err = loadJSON(migrationPath, &m); err != nil {
		o.Output = "migration unusable: " + err.Error()
		return o
	}
	o.MigrationID = m.ID
	if f.ID == "" || f.Table == "" || len(f.Columns) == 0 {
		o.Output = "fixture unusable: fixture, table, and columns are required"
		return o
	}
	if m.ID == "" || len(m.Ops) == 0 {
		o.Output = "migration unusable: migration and ops are required"
		return o
	}
	migrated, err := Apply(f, m)
	if err != nil {
		o.Output = "migration unusable: " + err.Error()
		return o
	}
	o.Ran = true
	msg, err := ReadPrevious(migrated)
	if err != nil {
		o.ExitCode = 1
		o.Output = fmt.Sprintf("%s after %s: %v", ReaderID, m.ID, err)
		return o
	}
	o.Output = fmt.Sprintf("%s after %s: %s", ReaderID, m.ID, msg)
	return o
}

// Request identifies the release and paths a surrogate record is for.
type Request struct {
	App, Component, Candidate  string
	FixturePath, MigrationPath string
	RestorePath                string
}

// Evidence runs the probe and returns a one-check evidence file. The exit code
// is taken from the run, never supplied; an unusable input records no exit
// code.
func Evidence(req Request) compat.Evidence {
	o := Probe(req.FixturePath, req.MigrationPath)
	c := compat.Check{
		Component:       req.Component,
		Reader:          compat.ReaderSurrogate,
		CandidateImage:  req.Candidate,
		Migration:       nonEmpty(o.MigrationID, req.MigrationPath),
		MigrationDigest: o.MigrationDigest,
		Fixture:         nonEmpty(o.FixtureID, req.FixturePath),
		FixtureDigest:   o.FixtureDigest,
		Probe: fmt.Sprintf("go run ./internal/compat/surrogate/cmd/surrogate-probe --fixture %s --migration %s",
			req.FixturePath, req.MigrationPath),
		Output:      o.Output,
		RestorePath: req.RestorePath,
	}
	if o.Ran {
		code := o.ExitCode
		c.ExitCode = &code
	}
	return compat.Evidence{Kind: compat.Kind, Version: compat.Version, App: req.App, Checks: []compat.Check{c}}
}

// loadJSON strictly decodes path into v and returns the file's sha256 digest.
func loadJSON(path string, v any) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return digest, fmt.Errorf("%s: %w", path, err)
	}
	// Exactly one JSON value: a second value or trailing garbage makes the
	// input unusable rather than silently ignored. Trailing whitespace is fine.
	if _, err := dec.Token(); err != io.EOF {
		return digest, fmt.Errorf("%s: unexpected data after the JSON value", path)
	}
	return digest, nil
}

func columnIndex(cols []Column, name string) int {
	for i, c := range cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// ofType reports whether v (as decoded from JSON) is a non-null value of t.
func ofType(v any, t string) bool {
	switch t {
	case "int":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "text":
		_, ok := v.(string)
		return ok
	}
	return false
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
