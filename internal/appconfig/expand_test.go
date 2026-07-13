package appconfig

import "testing"

// A plain app manifest (no components:) expands to just itself — the backward-
// compatible path every existing single-component file takes.
func TestExpand_SingleComponentIsIdentity(t *testing.T) {
	a := App{App: "x", Runtime: Runtime{Image: "img", Port: 8080}}
	got := a.Expand()
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].App != "x" || got[0].Runtime.Port != 8080 {
		t.Fatalf("identity expand altered the app: %+v", got[0])
	}
}

func TestExpand_MergeAndProvision(t *testing.T) {
	base := App{
		App:     "media",
		Runtime: Runtime{Image: "api-img", Port: 8000},
		Build:   BuildConfig{Submodules: []string{"vendor/x"}},
		Env:     map[string]string{"SHARED": "1"},
		Secrets: []string{"AWS_KEY"},
		DB:      []DataStore{{Name: "primary", Type: "postgres"}},
		Cache:   []DataStore{{Name: "events", Type: "redis"}},
		Components: []Component{
			{Component: "api", Env: map[string]string{"ROLE": "web"}, Secrets: []string{"EXTRA"}},
			{Component: "scanner", Port: intPtr(0), Env: map[string]string{"ROLE": "scanner"}},
			{Component: "ui", Runtime: &Runtime{Image: "ui-img", Port: 80}, DB: []DataStore{}, Cache: []DataStore{}},
		},
	}
	got := base.Expand()
	if len(got) != 3 {
		t.Fatalf("want 3 components, got %d", len(got))
	}
	api, scanner, ui := got[0], got[1], got[2]

	// Inheritance + per-key env merge.
	if api.Runtime.Image != "api-img" || api.Runtime.Port != 8000 {
		t.Errorf("api runtime = %+v (should inherit base)", api.Runtime)
	}
	if len(api.Build.Submodules) != 1 {
		t.Errorf("api should inherit base build.submodules, got %+v", api.Build)
	}
	if api.Env["SHARED"] != "1" || api.Env["ROLE"] != "web" {
		t.Errorf("api env merge = %+v", api.Env)
	}
	// Secrets union (base + component, deduped).
	if len(api.Secrets) != 2 {
		t.Errorf("api secrets union = %+v", api.Secrets)
	}

	// Port-only override keeps the base image.
	if scanner.Runtime.Image != "api-img" || scanner.Runtime.Port != 0 {
		t.Errorf("scanner runtime = %+v (port override must keep base image)", scanner.Runtime)
	}
	// Full runtime override replaces image + port.
	if ui.Runtime.Image != "ui-img" || ui.Runtime.Port != 80 {
		t.Errorf("ui runtime = %+v", ui.Runtime)
	}

	// Auto-provision: first user provisions; later sharers get provision:false; [] opts out.
	if !api.DB[0].Provisioned() {
		t.Errorf("api (first) should provision primary")
	}
	if scanner.DB[0].Provisioned() {
		t.Errorf("scanner should auto-share primary (provision:false)")
	}
	if !api.Cache[0].Provisioned() || scanner.Cache[0].Provisioned() {
		t.Errorf("cache provisioning: api should own, scanner share")
	}
	if len(ui.DB) != 0 || len(ui.Cache) != 0 {
		t.Errorf("ui opted out with []; got db=%+v cache=%+v", ui.DB, ui.Cache)
	}

	// The shared base slices must NOT be mutated by per-component provision flips.
	if !base.DB[0].Provisioned() || !base.Cache[0].Provisioned() {
		t.Errorf("Expand mutated the base store slices")
	}
}
