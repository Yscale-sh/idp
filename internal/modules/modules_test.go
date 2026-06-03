package modules

import (
	"strings"
	"testing"

	"github.com/jakenesler/platformctl/internal/clusterenv"
)

func cfg() *clusterenv.Config {
	c := &clusterenv.Config{
		Env: "dev",
		Argo: clusterenv.ArgoConfig{
			Namespace:      "argocd",
			RepoURL:        "https://github.com/jakenesler/platformctl.git",
			TargetRevision: "HEAD",
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
	src := keda.Application.Spec.Source
	if src.Chart != "keda" || src.RepoURL != "https://kedacore.github.io/charts" {
		t.Errorf("chartRepo source = %+v", src)
	}
	// chartRepo pins the version into targetRevision.
	if src.TargetRevision != "2.17.2" {
		t.Errorf("chartRepo targetRevision = %q, want 2.17.2", src.TargetRevision)
	}
	if keda.Application.Spec.Destination.Namespace != "keda" {
		t.Errorf("dest ns = %q", keda.Application.Spec.Destination.Namespace)
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
	src := pg.Application.Spec.Source
	if src.Path != "charts/infra/dev-postgres" {
		t.Errorf("localChart path = %q", src.Path)
	}
	if src.RepoURL != "https://github.com/jakenesler/platformctl.git" {
		t.Errorf("localChart repoURL should be the platform repo, got %q", src.RepoURL)
	}
	if src.Chart != "" {
		t.Errorf("localChart should not set chart name, got %q", src.Chart)
	}
}

func TestPlan_SyncPolicyAndLabels(t *testing.T) {
	planned, _ := Plan(cfg())
	p := planned[0]
	sp := p.Application.Spec.SyncPolicy
	if sp.Automated == nil || !sp.Automated.Prune || !sp.Automated.SelfHeal {
		t.Errorf("syncPolicy automated should prune+selfHeal: %+v", sp.Automated)
	}
	if p.Application.Metadata.Labels["platform/managed-by"] != "platformctl" {
		t.Error("module should carry platform/managed-by label")
	}
}

// Finding #2: an infra module that ships a LoadBalancer (via inline Helm values)
// must be REJECTED by the policy guardrail before any render/apply.
func TestCheckAll_RejectsLoadBalancerModule(t *testing.T) {
	c := &clusterenv.Config{
		Env: "dev",
		Argo: clusterenv.ArgoConfig{
			Namespace: "argocd", RepoURL: "https://example.com/r.git", TargetRevision: "HEAD",
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
		Argo: clusterenv.ArgoConfig{Namespace: "argocd", RepoURL: "https://example.com/r.git", TargetRevision: "HEAD"},
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
	y, err := planned[0].YAML("dev")
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if !strings.Contains(s, "kind: Application") {
		t.Error("YAML should be an Argo Application")
	}
	if !strings.Contains(s, "DO NOT EDIT") {
		t.Error("YAML should carry generated header")
	}
}
