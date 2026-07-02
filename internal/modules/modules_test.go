package modules

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/clusterenv"
)

func cfg() *clusterenv.Config {
	c := &clusterenv.Config{
		Env: "dev",
		Flux: clusterenv.FluxConfig{
			Namespace:  "flux-system",
			RepoURL:    "https://github.com/yscale-sh/idp.git",
			Branch:     "main",
			SourceName: "flux-system",
		},
		Modules: map[string]clusterenv.Module{
			"keda": {
				Enabled: true, Source: clusterenv.SourceChartRepo, Chart: "keda",
				RepoURL: "https://kedacore.github.io/charts", Version: "2.17.2", Namespace: "keda",
			},
			"dev-postgres": {
				Enabled: true, Source: clusterenv.SourceLocalChart,
				Chart: "charts/infra/dev-postgres", Namespace: "platform-data",
			},
			"yscale": {Enabled: false, Source: clusterenv.SourceChartRepo, Chart: "yscale", Version: "0.1.0"},
		},
	}
	c.ApplyDefaults()
	return c
}

func TestPlan_FiltersDisabledAndSorts(t *testing.T) {
	planned, err := Plan(cfg())
	if err != nil {
		t.Fatal(err)
	}
	if len(planned) != 2 {
		t.Fatalf("expected 2 enabled modules, got %d", len(planned))
	}
	// Sorted by name: dev-postgres before keda.
	if planned[0].Name != "dev-postgres" || planned[1].Name != "keda" {
		t.Errorf("unexpected order: %s, %s", planned[0].Name, planned[1].Name)
	}
	// yscale (disabled) is absent.
	for _, p := range planned {
		if p.Name == "yscale" {
			t.Error("disabled yscale should not be planned")
		}
	}
}

func TestPlan_ChartRepoSource(t *testing.T) {
	planned, _ := Plan(cfg())
	var keda PlannedModule
	for _, p := range planned {
		if p.Name == "keda" {
			keda = p
		}
	}
	cs := keda.HelmRelease.Spec.Chart.Spec
	// chartRepo: chart name + pinned version, sourceRef a HelmRepository in flux-system.
	if cs.Chart != "keda" || cs.Version != "2.17.2" {
		t.Errorf("chartRepo chart spec = %+v", cs)
	}
	if cs.SourceRef.Kind != "HelmRepository" || cs.SourceRef.Namespace != "flux-system" {
		t.Errorf("chartRepo sourceRef = %+v", cs.SourceRef)
	}
	if cs.SourceRef.Name != "kedacore" {
		t.Errorf("chartRepo sourceRef name = %q, want kedacore", cs.SourceRef.Name)
	}
	// A HelmRepository is emitted for the chart's repo.
	if keda.Repository == nil {
		t.Fatal("chartRepo module should emit a HelmRepository")
	}
	if keda.Repository.Spec.URL != "https://kedacore.github.io/charts" {
		t.Errorf("HelmRepository url = %q", keda.Repository.Spec.URL)
	}
	if keda.Repository.Metadata.Name != "kedacore" || keda.Repository.Metadata.Namespace != "flux-system" {
		t.Errorf("HelmRepository meta = %+v", keda.Repository.Metadata)
	}
	// targetNamespace is the module namespace with createNamespace true.
	if keda.HelmRelease.Spec.TargetNamespace != "keda" {
		t.Errorf("targetNamespace = %q", keda.HelmRelease.Spec.TargetNamespace)
	}
	if !keda.HelmRelease.Spec.Install.CreateNamespace {
		t.Error("install.createNamespace should be true")
	}
}

func TestPlan_LocalChartSource(t *testing.T) {
	planned, _ := Plan(cfg())
	var pg PlannedModule
	for _, p := range planned {
		if p.Name == "dev-postgres" {
			pg = p
		}
	}
	cs := pg.HelmRelease.Spec.Chart.Spec
	// localChart: chart path served by the GitRepository source, no version, no repo.
	if cs.Chart != "./charts/infra/dev-postgres" {
		t.Errorf("localChart chart path = %q", cs.Chart)
	}
	if cs.SourceRef.Kind != "GitRepository" || cs.SourceRef.Name != "flux-system" || cs.SourceRef.Namespace != "flux-system" {
		t.Errorf("localChart sourceRef = %+v", cs.SourceRef)
	}
	if cs.Version != "" {
		t.Errorf("localChart should not pin a version, got %q", cs.Version)
	}
	if pg.Repository != nil {
		t.Errorf("localChart should not emit a HelmRepository, got %+v", pg.Repository)
	}
}

func TestPlan_RemediationAndLabels(t *testing.T) {
	planned, _ := Plan(cfg())
	p := planned[0]
	sp := p.HelmRelease.Spec
	if sp.Install.Remediation == nil || sp.Install.Remediation.Retries != 3 {
		t.Errorf("install remediation should retry 3: %+v", sp.Install.Remediation)
	}
	if sp.Upgrade.Remediation == nil || sp.Upgrade.Remediation.Retries != 3 {
		t.Errorf("upgrade remediation should retry 3: %+v", sp.Upgrade.Remediation)
	}
	if p.HelmRelease.Metadata.Labels["platform/managed-by"] != "platformctl" {
		t.Error("module should carry platform/managed-by label")
	}
	if p.HelmRelease.Metadata.Namespace != "flux-system" {
		t.Errorf("HelmRelease should live in flux-system, got %q", p.HelmRelease.Metadata.Namespace)
	}
}

// Finding #2: an infra module that ships a LoadBalancer (via inline Helm values)
// must be REJECTED by the policy guardrail before any render/apply.
func TestCheckAll_RejectsLoadBalancerModule(t *testing.T) {
	c := &clusterenv.Config{
		Env: "dev",
		Flux: clusterenv.FluxConfig{
			Namespace: "flux-system", RepoURL: "https://example.com/r.git", Branch: "main", SourceName: "flux-system",
		},
		Modules: map[string]clusterenv.Module{
			"bad-lb": {
				Enabled: true, Source: clusterenv.SourceChartRepo, Chart: "bad-lb",
				RepoURL: "https://charts.example.com", Version: "1.0.0", Namespace: "bad-lb",
				Values: map[string]any{
					"service": map[string]any{"type": "LoadBalancer", "port": 80},
				},
			},
		},
	}
	c.ApplyDefaults()
	planned, err := Plan(c)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	err = CheckAll(planned, "", "dev")
	if err == nil {
		t.Fatal("expected a LoadBalancer module to be rejected")
	}
	if !strings.Contains(err.Error(), "LoadBalancer") {
		t.Errorf("expected LoadBalancer policy violation, got %v", err)
	}
}

// A module nesting the LoadBalancer deeper in its values tree is still caught.
func TestCheckAll_RejectsNestedLoadBalancerModule(t *testing.T) {
	c := &clusterenv.Config{
		Env:  "dev",
		Flux: clusterenv.FluxConfig{Namespace: "flux-system", RepoURL: "https://example.com/r.git", Branch: "main", SourceName: "flux-system"},
		Modules: map[string]clusterenv.Module{
			"bad-nested": {
				Enabled: true, Source: clusterenv.SourceChartRepo, Chart: "bad-nested",
				RepoURL: "https://charts.example.com", Version: "1.0.0", Namespace: "bad-nested",
				Values: map[string]any{
					"controller": map[string]any{
						"service": map[string]any{"type": "LoadBalancer"},
					},
				},
			},
		},
	}
	c.ApplyDefaults()
	planned, _ := Plan(c)
	if err := CheckAll(planned, "", "dev"); err == nil || !strings.Contains(err.Error(), "LoadBalancer") {
		t.Errorf("expected nested LoadBalancer rejection, got %v", err)
	}
}

// Clean modules (no LoadBalancer anywhere) pass the guardrail.
func TestCheckAll_AllowsCleanModules(t *testing.T) {
	if err := CheckAll(mustPlan(t), "", "dev"); err != nil {
		t.Errorf("clean modules should pass policy, got %v", err)
	}
}

func mustPlan(t *testing.T) []PlannedModule {
	t.Helper()
	planned, err := Plan(cfg())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return planned
}

func TestPlannedModule_YAML(t *testing.T) {
	planned, _ := Plan(cfg())
	var keda PlannedModule
	for _, p := range planned {
		if p.Name == "keda" {
			keda = p
		}
	}
	y, err := keda.YAML("dev")
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if !strings.Contains(s, "kind: HelmRelease") {
		t.Error("YAML should be a Flux HelmRelease")
	}
	if !strings.Contains(s, "kind: HelmRepository") {
		t.Error("chartRepo module YAML should also emit a HelmRepository")
	}
	if !strings.Contains(s, "helm.toolkit.fluxcd.io/v2") {
		t.Error("YAML should carry the Flux HelmRelease apiVersion")
	}
	if !strings.Contains(s, "DO NOT EDIT") {
		t.Error("YAML should carry generated header")
	}
}
