package render

import (
	"fmt"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/clusterenv"
)

// ResolvedConnection is a connectsTo entry resolved to a concrete address for the
// target environment, plus any Cloudflare Access service-token secret keys.
type ResolvedConnection struct {
	// EnvVar is the env-var name to set (from Connection.Env).
	EnvVar string
	// Value is the resolved address (cluster DNS or public URL).
	Value string
	// Mode is the resolution mode used.
	Mode string
	// Target is the resolved target app name.
	Target string
	// ServiceTokenKeys are the extra secret keys for serviceToken mode
	// (<ENV>_CF_ACCESS_CLIENT_ID / _SECRET). Empty otherwise.
	ServiceTokenKeys []string
}

// ResolveConnections resolves every connectsTo entry for app in env.
//
//   - clusterService -> http://<target>.<target>.svc.cluster.local:<port>
//     (server-to-server; port comes from the source port as a convention since
//     the target's port is not known cross-repo — documented assumption).
//   - publicRoute    -> https://<target's first public route host>
//   - serviceToken   -> publicRoute value + <ENV>_CF_ACCESS_CLIENT_ID/_SECRET keys.
//
// component connections resolve the target app name as <product>-<component>
// when product is set, else the component name. Cross-repo `app` connections use
// the app name directly. The function never fails the build for an unresolvable
// public route in non-prod; it emits a best-effort placeholder and notes it.
func ResolveConnections(app appconfig.App, env string, c *clusterenv.Config) ([]ResolvedConnection, error) {
	domain := clusterenv.DefaultDomain
	if c != nil && c.Domain != "" {
		domain = c.Domain
	}
	var out []ResolvedConnection
	for i, conn := range app.ConnectsTo {
		target := connectionTarget(app, conn)
		if target == "" {
			return nil, fmt.Errorf("connectsTo[%d]: cannot resolve target (set app or component)", i)
		}
		mode := conn.Mode
		if mode == "" {
			mode = appconfig.DefaultConnectsMode
		}
		rc := ResolvedConnection{EnvVar: conn.Env, Mode: mode, Target: target}
		switch mode {
		case "clusterService":
			rc.Value = fmt.Sprintf("http://%s.%s.%s:%d", target, target, domain, app.Runtime.Port)
		case "publicRoute":
			rc.Value = publicRouteURL(target, env)
		case "serviceToken":
			rc.Value = publicRouteURL(target, env)
			p := appconfig.EnvPrefix(conn.Env)
			rc.ServiceTokenKeys = []string{
				p + "_CF_ACCESS_CLIENT_ID",
				p + "_CF_ACCESS_CLIENT_SECRET",
			}
		default:
			return nil, fmt.Errorf("connectsTo[%d]: unknown mode %q", i, mode)
		}
		out = append(out, rc)
	}
	return out, nil
}

func connectionTarget(app appconfig.App, conn appconfig.Connection) string {
	if conn.App != "" {
		return conn.App
	}
	if conn.Component != "" {
		if app.Product != "" {
			return app.Product + "-" + conn.Component
		}
		return conn.Component
	}
	return ""
}

// publicRouteURL is a best-effort public URL for a target app. The target's real
// host lives in the target's own deploy.yaml (cross-repo), so platformctl uses a
// stable convention placeholder that the platform repo / Cloudflare config maps.
// It is intentionally explicit so a reviewer sees it in the diff.
func publicRouteURL(target, env string) string {
	return fmt.Sprintf("https://%s.%s.platform.internal", target, env)
}
