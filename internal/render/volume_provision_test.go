package render

import (
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
)

// TestProvisionedVsReferencedPVC proves the two type: pvc modes:
//   - size set, no claim  -> the platform PROVISIONS a PVC (named <workload>-<vol>)
//     and the volume references it; an entry lands in Values.ProvisionedClaims.
//   - claim set, no size  -> an existing PVC is referenced, no provisioning.
func TestProvisionedVsReferencedPVC(t *testing.T) {
	app := appconfig.App{
		App:     "datawork",
		Runtime: appconfig.Runtime{Image: "ghcr.io/yscale-sh/datawork", Port: 8080},
		Volumes: []appconfig.Volume{
			{Name: "data", Size: "20Gi", MountPath: "/data"},             // inferred: provisioned pvc
			{Name: "shared", Claim: "team-shared", MountPath: "/shared"}, // inferred: referenced pvc
		},
	}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	res, err := Render(app, "dev", devCluster(), "ghcr.io/yscale-sh/datawork:t1", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Exactly one provisioned claim (the sized volume), named per workload+vol.
	if got := len(res.Values.ProvisionedClaims); got != 1 {
		t.Fatalf("ProvisionedClaims = %d, want 1 (only the sized volume)", got)
	}
	claim := res.Values.ProvisionedClaims[0]
	wantName := app.Workload() + "-data"
	if claim["name"] != wantName || claim["size"] != "20Gi" {
		t.Errorf("provisioned claim = %v, want name=%s size=20Gi", claim, wantName)
	}

	// The volumes reference the right claim names: provisioned -> derived name,
	// referenced -> the existing claim verbatim.
	want := map[string]string{"data": wantName, "shared": "team-shared"}
	for _, v := range res.Values.Volumes {
		pvc, ok := v["persistentVolumeClaim"].(map[string]any)
		if !ok {
			t.Fatalf("volume %v has no persistentVolumeClaim", v["name"])
		}
		if pvc["claimName"] != want[v["name"].(string)] {
			t.Errorf("volume %v claimName = %v, want %v", v["name"], pvc["claimName"], want[v["name"].(string)])
		}
	}
}

// TestPVCValidation enforces the claim XOR size rule.
func TestPVCValidation(t *testing.T) {
	base := func(vol appconfig.Volume) appconfig.App {
		a := appconfig.App{App: "x", Runtime: appconfig.Runtime{Image: "ghcr.io/x/x", Port: 80}, Volumes: []appconfig.Volume{vol}}
		a.ApplyDefaults()
		return a
	}
	cases := []struct {
		name    string
		vol     appconfig.Volume
		wantErr bool
	}{
		{"inferred-provision", appconfig.Volume{Name: "d", Size: "5Gi", MountPath: "/d"}, false},
		{"inferred-reference", appconfig.Volume{Name: "d", Claim: "c", MountPath: "/d"}, false},
		{"inferred-emptydir", appconfig.Volume{Name: "d", MountPath: "/d"}, false},
		{"explicit-pvc-neither", appconfig.Volume{Name: "d", Type: "pvc", MountPath: "/d"}, true},
		{"explicit-pvc-both", appconfig.Volume{Name: "d", Type: "pvc", Size: "5Gi", Claim: "c", MountPath: "/d"}, true},
	}
	for _, tc := range cases {
		a := base(tc.vol)
		err := a.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: validate err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
