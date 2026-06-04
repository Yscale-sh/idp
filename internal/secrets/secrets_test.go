package secrets

import (
	"testing"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/clusterenv"
)

func TestPlan_BackendFromEnv(t *testing.T) {
	app := appconfig.App{App: "carshowdb"}

	devC := &clusterenv.Config{
		Secrets: clusterenv.SecretsConfig{
			Backend: clusterenv.BackendLocal, RefreshInterval: "1h",
			StoreRef: clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	dev := Plan(app, "dev", devC)
	if dev.Backend != clusterenv.BackendLocal || dev.StoreName != "platform-local" {
		t.Errorf("dev plan = %+v", dev)
	}
	if dev.AppRoot != "/apps/carshowdb/dev" {
		t.Errorf("dev AppRoot = %q", dev.AppRoot)
	}

	prodC := &clusterenv.Config{
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	prod := Plan(app, "prod", prodC)
	if prod.Backend != clusterenv.BackendSSM || prod.StoreName != "platform-ssm" {
		t.Errorf("prod plan = %+v", prod)
	}
	if prod.AppRoot != "/apps/carshowdb/prod" {
		t.Errorf("prod AppRoot = %q", prod.AppRoot)
	}
}

func TestSharedPathAndPin(t *testing.T) {
	if got := SharedPath(GroupStripe, "STRIPE_SECRET_KEY"); got != "/shared/stripe/STRIPE_SECRET_KEY" {
		t.Errorf("SharedPath = %q", got)
	}
	app := appconfig.App{App: "carshowdb"}
	s := Plan(app, "prod", nil)
	s.PinShared(GroupSendgrid, "SENDGRID_API_KEY", "SENDGRID_API_KEY")
	if len(s.SharedRefs) != 1 || s.SharedRefs[0].RemoteKey != "/shared/sendgrid/SENDGRID_API_KEY" {
		t.Errorf("PinShared = %+v", s.SharedRefs)
	}
}
