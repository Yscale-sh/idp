package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"sigs.k8s.io/yaml"
)

func TestToAppEntry_RollbackNilOmitsKey(t *testing.T) {
	app := appconfig.App{App: "x"}
	e := (&Result{App: app, Env: "dev"}).ToAppEntry()
	if e.Rollback != nil {
		t.Fatalf("Rollback = %+v, want nil", e.Rollback)
	}
	b, err := yaml.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "rollback") {
		t.Fatalf("entry without a rollback block must not emit the key:\n%s", b)
	}
}

func TestUmbrellaRollbackRemediation(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed; required by ci-local in strict mode")
	}
	for _, env := range []string{"dev", "prod"} {
		for _, strategy := range []string{"", "auto", "manual"} {
			t.Run(env+"/"+strategy, func(t *testing.T) {
				entry := AppEntry{Name: "audit", Namespace: "audit-" + env, ReleaseName: "audit", Values: map[string]any{}}
				if strategy != "" {
					entry.Rollback = &RollbackEntry{Strategy: strategy, DataCompatible: strategy != "manual", Note: "migration drops a column"}
				}
				raw, err := yaml.Marshal(map[string]any{"env": env, "apps": []AppEntry{entry}})
				if err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(file, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command(helm, "template", "audit", "../../charts/cluster", "--show-only", "templates/apps.yaml", "-f", file).CombinedOutput()
				if err != nil {
					t.Fatalf("helm: %v: %s", err, out)
				}
				var hr HelmRelease
				if err := yaml.Unmarshal(out, &hr); err != nil {
					t.Fatal(err)
				}
				rem := hr.Spec.Upgrade.Remediation
				if rem == nil {
					t.Fatal("missing upgrade remediation")
				}
				if strategy == "manual" {
					if rem.Retries != 0 || rem.RemediateLastFailure == nil || *rem.RemediateLastFailure {
						t.Fatalf("manual release permits remediation: %+v", rem)
					}
				} else if rem.Retries != 3 || rem.RemediateLastFailure != nil {
					t.Fatalf("default remediation changed: %+v", rem)
				}
			})
		}
	}
}

func TestUmbrellaRejectsUnsafeRollback(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed")
	}
	for _, rollback := range []string{
		"{strategy: auto, dataCompatible: false, note: drops a column}",
		"{dataCompatible: false, note: drops a column}",
		"{strategy: typo, dataCompatible: true}",
		"{strategy: manual, dataCompatible: false}",
		"{strategy: manual, dataCompatible: false, note: ' '}",
	} {
		file := filepath.Join(t.TempDir(), "values.yaml")
		raw := "env: prod\napps:\n  - name: audit\n    releaseName: audit\n    namespace: audit-prod\n    values: {}\n    rollback: " + rollback + "\n"
		if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(helm, "template", "audit", "../../charts/cluster", "-f", file).CombinedOutput()
		if err == nil {
			t.Fatalf("unsafe rollback accepted: %s\n%s", rollback, out)
		}
		if !strings.Contains(string(out), "rollback") {
			t.Fatalf("unrelated template failure: %s", out)
		}
	}
}

func TestToAppEntry_RollbackResolved(t *testing.T) {
	f := false
	app := appconfig.App{App: "x", Rollback: &appconfig.Rollback{DataCompatible: &f, Note: "drops a column"}}
	e := (&Result{App: app, Env: "dev"}).ToAppEntry()
	want := RollbackEntry{Strategy: "manual", DataCompatible: false, Note: "drops a column"}
	if e.Rollback == nil || *e.Rollback != want {
		t.Fatalf("Rollback = %+v, want %+v", e.Rollback, want)
	}
}

func TestBuildHelmRelease_ManualRollbackDisablesUpgradeRemediation(t *testing.T) {
	retries := func(a appconfig.App) (int, int) {
		hr := BuildHelmRelease(a, "dev", nil, Values{})
		return hr.Spec.Upgrade.Remediation.Retries, hr.Spec.Install.Remediation.Retries
	}
	if up, in := retries(appconfig.App{App: "x"}); up != 3 || in != 3 {
		t.Errorf("no block: upgrade=%d install=%d, want 3/3", up, in)
	}
	if up, _ := retries(appconfig.App{App: "x", Rollback: &appconfig.Rollback{Strategy: "auto"}}); up != 3 {
		t.Errorf("auto: upgrade retries=%d, want 3", up)
	}
	if up, in := retries(appconfig.App{App: "x", Rollback: &appconfig.Rollback{Strategy: "manual"}}); up != 0 || in != 3 {
		t.Errorf("manual: upgrade=%d install=%d, want 0/3", up, in)
	}
}
