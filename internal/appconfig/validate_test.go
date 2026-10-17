package appconfig

import (
	"errors"
	"strings"
	"testing"
)

func validApp() App {
	return App{
		App:     "carshowdb",
		Runtime: Runtime{Image: "ghcr.io/yscale-sh/carshowdb-api", Port: 8080},
	}
}

func TestValidate_Table(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*App)
		wantErr  bool
		wantPart string // substring expected in the aggregated error
	}{
		{
			name:    "valid minimal",
			mutate:  func(a *App) {},
			wantErr: false,
		},
		{
			name:     "missing app",
			mutate:   func(a *App) { a.App = "" },
			wantErr:  true,
			wantPart: "app: is required",
		},
		{
			name:     "app not dns1123 (uppercase)",
			mutate:   func(a *App) { a.App = "CarShowDB" },
			wantErr:  true,
			wantPart: "DNS-1123",
		},
		{
			name:     "missing image",
			mutate:   func(a *App) { a.Runtime.Image = "" },
			wantErr:  true,
			wantPart: "runtime.image: is required",
		},
		{
			name:     "image carries a tag",
			mutate:   func(a *App) { a.Runtime.Image = "ghcr.io/yscale-sh/carshowdb-api:latest" },
			wantErr:  true,
			wantPart: "repository only",
		},
		{
			name:     "port out of range",
			mutate:   func(a *App) { a.Runtime.Port = 70000 },
			wantErr:  true,
			wantPart: "runtime.port",
		},
		{
			name:     "build context rejects shell input",
			mutate:   func(a *App) { a.Build.Context = "ui; touch /tmp/pwned" },
			wantErr:  true,
			wantPart: "build.context",
		},
		{
			name:     "dockerfile rejects traversal",
			mutate:   func(a *App) { a.Build.Dockerfile = "../Dockerfile" },
			wantErr:  true,
			wantPart: "build.dockerfile",
		},
		{
			name: "submodule rejects option injection",
			mutate: func(a *App) {
				a.Build.Submodules = []string{"--config=protocol.file.allow=always"}
			},
			wantErr:  true,
			wantPart: "build.submodules[0]",
		},
		{
			name:     "bad profile",
			mutate:   func(a *App) { a.Sizing.Profile = "gigantic" },
			wantErr:  true,
			wantPart: "sizing.profile",
		},
		{
			name:     "bad db type",
			mutate:   func(a *App) { a.DB = []DataStore{{Name: "primary", Type: "cassandra"}} },
			wantErr:  true,
			wantPart: "db[0].type",
		},
		{
			name: "duplicate db name",
			mutate: func(a *App) {
				a.DB = []DataStore{{Name: "primary", Type: "postgres"}, {Name: "primary", Type: "postgres"}}
			},
			wantErr:  true,
			wantPart: "duplicate db name",
		},
		{
			name:     "bad storage type",
			mutate:   func(a *App) { a.Storage = []Storage{{Name: "uploads", Type: "gcs"}} },
			wantErr:  true,
			wantPart: "storage[0].type",
		},
		{
			name:     "connectsTo missing target",
			mutate:   func(a *App) { a.ConnectsTo = []Connection{{Env: "API_URL"}} },
			wantErr:  true,
			wantPart: "must set either app or component",
		},
		{
			name:     "connectsTo missing env",
			mutate:   func(a *App) { a.ConnectsTo = []Connection{{App: "other"}} },
			wantErr:  true,
			wantPart: "connectsTo[0].env: is required",
		},
		{
			name:     "bad connect mode",
			mutate:   func(a *App) { a.ConnectsTo = []Connection{{App: "other", Env: "X", Mode: "rpc"}} },
			wantErr:  true,
			wantPart: "connectsTo[0].mode",
		},
		{
			name: "scale-to-zero requires HTTPScaledObject",
			mutate: func(a *App) {
				a.Sizing.Autoscale = Autoscale{Enabled: true, Min: 0, Max: 5, Kind: DefaultAutoscaleK}
			},
			wantErr:  true,
			wantPart: "scale-to-zero",
		},
		{
			name: "autoscale min>max",
			mutate: func(a *App) {
				a.Sizing.Autoscale = Autoscale{Enabled: true, Min: 5, Max: 2, Kind: DefaultAutoscaleK}
			},
			wantErr:  true,
			wantPart: "min (5) must be <= max (2)",
		},
		{
			name:     "metrics path missing slash",
			mutate:   func(a *App) { a.Metrics.Path = "metrics" },
			wantErr:  true,
			wantPart: "metrics.path",
		},
		{
			name: "valid with everything",
			mutate: func(a *App) {
				a.Product = "carshow"
				a.Component = "api"
				a.Routes = []Route{{Host: "carshowdb.example.com", Public: true}}
				a.Sizing = Sizing{Profile: "small", Replicas: intPtr(2), Autoscale: Autoscale{Enabled: true, Min: 2, Max: 5}}
				a.DB = []DataStore{{Name: "primary", Type: "postgres", Size: "minimal"}}
				a.Cache = []DataStore{{Name: "sessions", Type: "redis"}}
				a.Storage = []Storage{{Name: "uploads", Type: "r2", Bucket: "carshowdb-uploads"}}
				a.ConnectsTo = []Connection{{App: "other", Env: "API_URL", Mode: "publicRoute"}}
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validApp()
			tc.mutate(&a)
			err := a.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && tc.wantPart != "" && !strings.Contains(err.Error(), tc.wantPart) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantPart)
			}
		})
	}
}

func TestValidationErrors_Aggregate(t *testing.T) {
	a := App{} // missing app + runtime
	err := a.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	var verrs ValidationErrors
	if !errors.As(err, &verrs) {
		t.Fatalf("error is not ValidationErrors: %T", err)
	}
	if len(verrs) < 2 {
		t.Fatalf("expected multiple aggregated errors, got %d: %v", len(verrs), verrs)
	}
}

func TestValidate_LoggingRetention(t *testing.T) {
	base := func() App {
		a := App{App: "x", Runtime: Runtime{Image: "ghcr.io/e/x", Port: 80}}
		a.ApplyDefaults()
		return a
	}
	for _, ok := range []string{"", "90d", "12h", "365d"} {
		a := base()
		a.Logging.Retention = ok
		if err := a.Validate(); err != nil {
			t.Errorf("retention %q should be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"90", "0d", "90 days", "d90", "1.5d", "90m"} {
		a := base()
		a.Logging.Retention = bad
		if err := a.Validate(); err == nil {
			t.Errorf("retention %q should be rejected", bad)
		}
	}
}

// intPtr returns a pointer to i — for setting Sizing.Replicas (a *int so an
// explicit 0 is distinguishable from unset) in table-driven tests.
func intPtr(i int) *int { return &i }

func TestValidate_Rollback(t *testing.T) {
	f := false
	mk := func(r *Rollback) App {
		a := App{App: "x", Runtime: Runtime{Image: "ghcr.io/e/x", Port: 80}, Rollback: r}
		a.ApplyDefaults()
		return a
	}
	good := []*Rollback{nil, {}, {Strategy: "auto"}, {Strategy: "manual"},
		{DataCompatible: &f, Note: "drops users.legacy_id"}}
	for i, r := range good {
		a := mk(r)
		if err := a.Validate(); err != nil {
			t.Errorf("good[%d] rejected: %v", i, err)
		}
	}
	bad := map[string]*Rollback{
		"rollback.strategy": {Strategy: "yolo"},
		"rollback.note":     {DataCompatible: &f},
	}
	for field, r := range bad {
		a := mk(r)
		err := a.Validate()
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want error naming field, got %v", field, err)
		}
	}
	for _, note := range []string{"two\nlines", strings.Repeat("a", 201)} {
		a := mk(&Rollback{Note: note})
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "rollback.note") {
			t.Errorf("note %q should be rejected, got %v", note, err)
		}
	}
	a := mk(&Rollback{DataCompatible: &f, Note: "   "})
	if err := a.Validate(); err == nil {
		t.Error("blank note with dataCompatible=false should be rejected")
	}
}

func TestValidate_RollbackAutoCannotBeDataIncompatible(t *testing.T) {
	f := false
	a := App{App: "x", Runtime: Runtime{Image: "ghcr.io/e/x", Port: 80},
		Rollback: &Rollback{Strategy: "auto", DataCompatible: &f, Note: "drops a table"}}
	a.ApplyDefaults()
	err := a.Validate()
	if err == nil || !strings.Contains(err.Error(), "rollback.strategy") {
		t.Fatalf("auto + dataCompatible:false must be rejected, got %v", err)
	}
}

func TestValidate_RollbackNoteLimitCountsCharacters(t *testing.T) {
	note := strings.Repeat("é", 150) // 150 characters, 300 bytes
	a := App{App: "x", Runtime: Runtime{Image: "ghcr.io/e/x", Port: 80}, Rollback: &Rollback{Note: note}}
	a.ApplyDefaults()
	if err := a.Validate(); err != nil {
		t.Fatalf("a 150-character note was rejected: %v", err)
	}
	a.Rollback.Note = strings.Repeat("é", 201)
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "rollback.note") {
		t.Fatalf("a 201-character note was accepted: %v", err)
	}
}
