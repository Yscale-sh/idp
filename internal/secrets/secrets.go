// Package secrets generates the per-environment ExternalSecret wiring the app
// chart consumes. The backend is chosen per environment (CONVENTIONS.md §4):
//
//   - dev / on-prem / local -> backend "local" (external-secrets Kubernetes
//     provider, or a plain in-cluster Secret for dev); storeRef -> the env's
//     local SecretStore (e.g. platform-local).
//   - prod / cloud -> backend "ssm" (AWS SSM via external-secrets); storeRef ->
//     the env's SSM ClusterSecretStore (e.g. platform-ssm).
//
// SSM path convention: app secrets at /apps/<app>/<env>/...; shared groups at
// /shared/<group>/* (stripe, sendgrid, storage, google-oauth).
//
// This package emits NO real secret values — only references (remoteRefs) and
// the dataFrom pull of the app's SSM root. It is OSS-clean by construction.
package secrets

import (
	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// SharedGroup is a Tier-C shared secret group stored once under /shared/<group>/*.
type SharedGroup string

const (
	GroupStripe      SharedGroup = "stripe"
	GroupSendgrid    SharedGroup = "sendgrid"
	GroupStorage     SharedGroup = "storage"
	GroupGoogleOAuth SharedGroup = "google-oauth"
)

// SharedPath returns the SSM path for a key within a shared group:
// /shared/<group>/<key>.
func SharedPath(group SharedGroup, key string) string {
	return "/shared/" + string(group) + "/" + key
}

// Spec is the resolved ExternalSecret plan for an app in an env. It mirrors the
// values.yaml externalSecret block and is what the renderer embeds. The plan
// references the app's SSM root (dataFrom) plus pinned shared-group keys
// (remoteRefs); it never carries secret material.
type Spec struct {
	Enabled         bool
	Backend         string
	RefreshInterval string
	StoreName       string
	StoreKind       string
	// AppRoot is the SSM root the whole-secret dataFrom pulls (/apps/<app>/<env>).
	AppRoot string
	// SharedRefs are pinned shared-group keys (secretKey -> remote SSM path).
	SharedRefs []SharedRef
}

// SharedRef pins one shared-group secret key into the runtime Secret.
type SharedRef struct {
	SecretKey string
	RemoteKey string
}

// Plan computes the ExternalSecret spec for app in env using the env's backend.
// sharedGroups lists which Tier-C groups this app references (e.g. stripe,
// sendgrid). Only the group path roots are pinned via remoteRefs at the value
// layer through dataFrom-by-group; individual keys are added by callers that
// know the exact key list (e.g. carshowdb wiring).
func Plan(app appconfig.App, env string, c *clusterenv.Config) Spec {
	s := Spec{
		Enabled:         true,
		AppRoot:         app.SSMRoot(env),
		RefreshInterval: clusterenv.DefaultRefresh,
		Backend:         clusterenv.BackendLocal,
		StoreKind:       clusterenv.KindClusterSecretStore,
	}
	if c != nil {
		s.Backend = c.Secrets.Backend
		s.StoreName = c.Secrets.StoreRef.Name
		s.StoreKind = c.Secrets.StoreRef.Kind
		if c.Secrets.RefreshInterval != "" {
			s.RefreshInterval = c.Secrets.RefreshInterval
		}
	}
	return s
}

// PinShared adds pinned shared-group key references to the spec. secretKey is the
// env-var name materialized in the runtime Secret; key is the key within the
// shared group's SSM path.
func (s *Spec) PinShared(group SharedGroup, secretKey, key string) {
	s.SharedRefs = append(s.SharedRefs, SharedRef{
		SecretKey: secretKey,
		RemoteKey: SharedPath(group, key),
	})
}
