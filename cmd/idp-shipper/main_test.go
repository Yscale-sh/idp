package main

import "testing"

func TestUnderDir(t *testing.T) {
	cases := []struct {
		file, dir string
		want      bool
	}{
		{"main.go", ".", true},       // root context contains everything
		{"ui/app.tsx", "", true},     // empty dir == root
		{"ui/app.tsx", "ui", true},   // direct child
		{"ui/src/x.ts", "ui", true},  // nested child
		{"ui/app.tsx", "ui/", true},  // trailing slash on dir tolerated
		{"api/main.go", "ui", false}, // sibling dir is not under ui
		{"uixyz/a", "ui", false},     // prefix-only, not a path boundary
		{"ui", "ui", true},           // the dir itself
	}
	for _, c := range cases {
		if got := underDir(c.file, c.dir); got != c.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", c.file, c.dir, got, c.want)
		}
	}
}

func TestUnderAny(t *testing.T) {
	subs := []string{"vendor/yscale-transcode", "third_party/x"}
	cases := []struct {
		file string
		want bool
	}{
		{"vendor/yscale-transcode", true},            // gitlink pointer bump
		{"vendor/yscale-transcode/src/lib.rs", true}, // file inside the submodule
		{"third_party/x/go.mod", true},
		{"vendor/other/y", false},
		{"main.go", false},
	}
	for _, c := range cases {
		if got := underAny(c.file, subs); got != c.want {
			t.Errorf("underAny(%q) = %v, want %v", c.file, got, c.want)
		}
	}
	if underAny("anything", nil) {
		t.Error("underAny with no submodules must be false")
	}
}

func TestBuildAffected(t *testing.T) {
	deployFiles := []string{"deploy/yscale-media.deploy.yaml"}

	// A multi-image repo: api built from root, ui built from the ui/ subdir.
	api := buildTarget{Image: "ghcr.io/x/api", Context: ".", Submodules: []string{"vendor/transcode"}}
	ui := buildTarget{Image: "ghcr.io/x/ui", Context: "ui"}

	// Config-only change: the shopping list is not a build input → nothing rebuilds.
	if buildAffected([]string{"deploy/yscale-media.deploy.yaml"}, api, deployFiles) {
		t.Error("deploy.yaml change must not affect the api build")
	}
	if buildAffected([]string{"deploy/yscale-media.deploy.yaml"}, ui, deployFiles) {
		t.Error("deploy.yaml change must not affect the ui build")
	}

	// A change confined to ui/ rebuilds ui but NOT the root-context api... except
	// the api context is "." so it conservatively sees everything. The real win is
	// the reverse: a backend change must not rebuild ui.
	if buildAffected([]string{"src/server.rs"}, ui, deployFiles) {
		t.Error("backend change must not affect the ui build")
	}
	if !buildAffected([]string{"ui/index.html"}, ui, deployFiles) {
		t.Error("ui change must affect the ui build")
	}

	// Submodule bump affects only the image that vendors it.
	if !buildAffected([]string{"vendor/transcode"}, api, deployFiles) {
		t.Error("submodule bump must affect the api build")
	}
	if buildAffected([]string{"vendor/transcode"}, ui, deployFiles) {
		t.Error("submodule bump must not affect the ui build (it doesn't vendor it)")
	}

	// Root-context image conservatively rebuilds on any non-deploy change.
	if !buildAffected([]string{"src/server.rs"}, api, deployFiles) {
		t.Error("source change must affect the root-context api build")
	}

	// Mixed change set: deploy.yaml + a ui file → ui affected, decided by the ui file.
	if !buildAffected([]string{"deploy/yscale-media.deploy.yaml", "ui/x.ts"}, ui, deployFiles) {
		t.Error("ui file in a mixed change set must affect the ui build")
	}
}
