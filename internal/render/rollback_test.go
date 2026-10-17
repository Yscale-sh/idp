package render

import (
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
