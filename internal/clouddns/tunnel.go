package clouddns

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// This file is the Cloudflare Tunnel LIFECYCLE — the account-scoped half of the
// "auto-registration" that lets a developer go from just (a domain + a container)
// to a live, tunnelled app with no manual Cloudflare steps. It mirrors what the
// carshowdatabase prod_api.yml workflow does by hand in curl:
//
//	EnsureTunnel  -> create the named tunnel (idempotent) and return its id
//	TunnelToken   -> mint the connector token the cloudflared sidecar runs with
//	SetIngress    -> push the remote-managed ingress (hostname -> localhost:port)
//	(+ SyncHosts in cloudflare.go writes the proxied CNAME -> <id>.cfargotunnel.com)
//
// Tunnel endpoints are ACCOUNT-scoped (/accounts/<acct>/cfd_tunnel...), unlike the
// zone-scoped DNS records, so these take an accountID.

// Tunnel is a Cloudflare named tunnel.
type Tunnel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// IngressRule is one cloudflared ingress entry. A rule with an empty Hostname is
// the catch-all (Cloudflare requires the ingress list to end with one; we use
// service "http_status:404").
type IngressRule struct {
	Hostname string `json:"hostname,omitempty"`
	Service  string `json:"service"`
}

// CatchAll is the required terminal ingress rule.
func CatchAll() IngressRule { return IngressRule{Service: "http_status:404"} }

// EnsureTunnel returns the id of the named tunnel under accountID, creating it
// with a fresh random tunnel_secret when absent. Idempotent by name (so a re-run
// adopts the existing tunnel instead of making a duplicate). With dryRun it only
// looks up — it never creates — returning created=false and an empty id if absent.
func (c *Client) EnsureTunnel(accountID, name string, dryRun bool) (id string, created bool, err error) {
	q := url.Values{}
	q.Set("name", name)
	q.Set("is_deleted", "false")
	resp, err := c.do(http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel?"+q.Encode(), nil)
	if err != nil {
		return "", false, err
	}
	var existing []Tunnel
	if err := json.Unmarshal(resp.Result, &existing); err != nil {
		return "", false, fmt.Errorf("decode tunnels: %w", err)
	}
	if len(existing) > 0 && existing[0].ID != "" {
		return existing[0].ID, false, nil
	}
	if dryRun {
		return "", false, nil
	}
	secret, err := randomSecret()
	if err != nil {
		return "", false, err
	}
	resp, err = c.do(http.MethodPost, "/accounts/"+accountID+"/cfd_tunnel",
		map[string]string{"name": name, "tunnel_secret": secret})
	if err != nil {
		return "", false, err
	}
	var t Tunnel
	if err := json.Unmarshal(resp.Result, &t); err != nil {
		return "", false, fmt.Errorf("decode created tunnel: %w", err)
	}
	if t.ID == "" {
		return "", false, fmt.Errorf("cloudflare created a tunnel with no id")
	}
	return t.ID, true, nil
}

// TunnelToken mints the connector token for a tunnel — the value the cloudflared
// sidecar runs with (TUNNEL_TOKEN). It is base64 JSON {a,t,s}; TunnelIDFromToken
// can recover the tunnel id from it.
func (c *Client) TunnelToken(accountID, tunnelID string) (string, error) {
	resp, err := c.do(http.MethodGet, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/token", nil)
	if err != nil {
		return "", err
	}
	var token string
	if err := json.Unmarshal(resp.Result, &token); err != nil {
		return "", fmt.Errorf("decode tunnel token: %w", err)
	}
	if token == "" {
		return "", fmt.Errorf("cloudflare returned an empty tunnel token")
	}
	return token, nil
}

// SetIngress writes the tunnel's remote-managed ingress config. A token-mode
// cloudflared reads its routing from Cloudflare (not a local config.yaml), so this
// is what actually wires hostname -> the app's local service. The rules should end
// with CatchAll(). With dryRun it does nothing.
func (c *Client) SetIngress(accountID, tunnelID string, rules []IngressRule, dryRun bool) error {
	if dryRun {
		return nil
	}
	body := map[string]any{"config": map[string]any{"ingress": rules}}
	_, err := c.do(http.MethodPut, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID+"/configurations", body)
	return err
}

// DeleteTunnel removes a named tunnel (teardown). Cloudflare rejects deletion while
// the tunnel still has active connections, so callers should tear down the workload
// (and its cloudflared sidecar) first. With dryRun it does nothing.
func (c *Client) DeleteTunnel(accountID, tunnelID string, dryRun bool) error {
	if dryRun {
		return nil
	}
	_, err := c.do(http.MethodDelete, "/accounts/"+accountID+"/cfd_tunnel/"+tunnelID, nil)
	return err
}

// FindTunnel returns the id of the named tunnel, or "" if none (no create).
func (c *Client) FindTunnel(accountID, name string) (string, error) {
	id, _, err := c.EnsureTunnel(accountID, name, true /* dryRun = lookup only */)
	return id, err
}

// randomSecret is a base64 of 32 random bytes — the tunnel_secret a new tunnel is
// created with (matches `openssl rand -base64 32`).
func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate tunnel secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
