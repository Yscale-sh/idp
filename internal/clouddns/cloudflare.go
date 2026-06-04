// Package clouddns is the OPTIONAL DNS step of the ship pipeline: it reconciles
// the public DNS records for an app's public routes so they resolve THROUGH the
// app's Cloudflare Tunnel. Exposure is always the tunnel (the cloudflared
// sidecar) — this package never creates a LoadBalancer or an Ingress; it only
// upserts a proxied CNAME <host> -> <tunnelID>.cfargotunnel.com so a browser can
// find the tunnel. Setting that record by hand (Cloudflare dashboard) is the
// default; `jdpctl dns sync` automates it when an operator opts in.
//
// It talks to the Cloudflare API only (Bearer token), never the cluster, and is
// dependency-free (standard library). The tunnel id is derived from the
// cloudflared TUNNEL_TOKEN, so the only extra credential needed is a zone
// DNS:Edit API token.
package clouddns

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the Cloudflare v4 API root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// TunnelDomain is the CNAME-target suffix for a Cloudflare Tunnel: a public
// hostname is a proxied CNAME to <tunnelID>.cfargotunnel.com.
const TunnelDomain = "cfargotunnel.com"

// Client is a minimal Cloudflare API client.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// New builds a client from a zone DNS:Edit API token.
func New(token string) *Client {
	return &Client{
		Token:   token,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Zone is a Cloudflare zone (DNS domain).
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Record is a Cloudflare DNS record (we only manage CNAMEs here).
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Action is what a reconcile did (or, in dry-run, would do) to one record.
type Action string

const (
	ActionCreated   Action = "created"
	ActionUpdated   Action = "updated"
	ActionUnchanged Action = "unchanged"
	ActionDeleted   Action = "deleted"
	ActionAbsent    Action = "absent"
)

// Result is the per-host outcome of a SyncHosts call.
type Result struct {
	Host   string
	Zone   string
	Action Action
	Err    error
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type resultInfo struct {
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

type apiResponse struct {
	Success    bool            `json:"success"`
	Errors     []apiError      `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *resultInfo     `json:"result_info,omitempty"`
}

// do issues one API call and returns the parsed envelope, failing on a non-success
// body. body (if non-nil) is JSON-encoded; the caller unmarshals resp.Result.
func (c *Client) do(method, path string, body any) (*apiResponse, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return nil, fmt.Errorf("cloudflare %s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if !ar.Success {
		return nil, fmt.Errorf("cloudflare %s %s: %s", method, path, formatErrors(ar.Errors, resp.StatusCode))
	}
	return &ar, nil
}

func formatErrors(errs []apiError, status int) string {
	if len(errs) == 0 {
		return fmt.Sprintf("status %d (no error detail)", status)
	}
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, fmt.Sprintf("%d %s", e.Code, e.Message))
	}
	return strings.Join(parts, "; ")
}

// ListZones returns every zone the token can see (following pagination).
func (c *Client) ListZones() ([]Zone, error) {
	var all []Zone
	for page := 1; ; page++ {
		resp, err := c.do(http.MethodGet, fmt.Sprintf("/zones?per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		var batch []Zone
		if err := json.Unmarshal(resp.Result, &batch); err != nil {
			return nil, fmt.Errorf("decode zones: %w", err)
		}
		all = append(all, batch...)
		if resp.ResultInfo == nil || resp.ResultInfo.TotalPages <= page {
			break
		}
	}
	return all, nil
}

// ZoneForHost returns the most specific zone that owns host — the zone whose name
// equals host or is a dot-suffix of it, preferring the longest match (so a
// delegated subdomain zone wins over its parent). ok is false if none match.
func ZoneForHost(zones []Zone, host string) (Zone, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	var best Zone
	found := false
	for _, z := range zones {
		name := strings.TrimSuffix(strings.ToLower(z.Name), ".")
		if host == name || strings.HasSuffix(host, "."+name) {
			if !found || len(name) > len(best.Name) {
				best = z
				found = true
			}
		}
	}
	return best, found
}

func zoneNames(zones []Zone) string {
	if len(zones) == 0 {
		return "(none visible to this API token)"
	}
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		names = append(names, z.Name)
	}
	return strings.Join(names, ", ")
}

// findCNAME returns the existing CNAME for name in a zone, or nil if absent.
func (c *Client) findCNAME(zoneID, name string) (*Record, error) {
	q := url.Values{}
	q.Set("type", "CNAME")
	q.Set("name", strings.TrimSuffix(strings.ToLower(name), "."))
	resp, err := c.do(http.MethodGet, "/zones/"+zoneID+"/dns_records?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var recs []Record
	if err := json.Unmarshal(resp.Result, &recs); err != nil {
		return nil, fmt.Errorf("decode dns_records: %w", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return &recs[0], nil
}

// UpsertCNAME ensures a proxied CNAME name -> content exists in the zone. It
// returns the action taken: created (new), updated (content/proxy changed), or
// unchanged. With dryRun it reads but never writes, returning the action it WOULD
// take. proxied should be true so traffic flows through Cloudflare to the tunnel.
func (c *Client) UpsertCNAME(zoneID, name, content string, proxied bool, comment string, dryRun bool) (Action, error) {
	existing, err := c.findCNAME(zoneID, name)
	if err != nil {
		return "", err
	}
	rec := Record{Type: "CNAME", Name: name, Content: content, Proxied: proxied, TTL: 1, Comment: comment}
	if existing == nil {
		if !dryRun {
			if _, err := c.do(http.MethodPost, "/zones/"+zoneID+"/dns_records", rec); err != nil {
				return "", err
			}
		}
		return ActionCreated, nil
	}
	if existing.Content == content && existing.Proxied == proxied {
		return ActionUnchanged, nil
	}
	if !dryRun {
		if _, err := c.do(http.MethodPut, "/zones/"+zoneID+"/dns_records/"+existing.ID, rec); err != nil {
			return "", err
		}
	}
	return ActionUpdated, nil
}

// DeleteCNAME removes the CNAME for name (teardown). Returns deleted, or absent
// when there was nothing to delete. With dryRun it reads but never writes.
func (c *Client) DeleteCNAME(zoneID, name string, dryRun bool) (Action, error) {
	existing, err := c.findCNAME(zoneID, name)
	if err != nil {
		return "", err
	}
	if existing == nil {
		return ActionAbsent, nil
	}
	if !dryRun {
		if _, err := c.do(http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+existing.ID, nil); err != nil {
			return "", err
		}
	}
	return ActionDeleted, nil
}

// SyncHosts reconciles each host to point at target (the tunnel CNAME). When del
// is true it deletes the records instead (teardown). When zoneID is non-empty it
// writes every host into THAT zone (no ListZones call) — needed because a DNS:Edit
// API token is often scoped to a single zone and so cannot list zones; the zone id
// is the one in SSM beside the token. When zoneID is empty it lists zones once and
// maps each host to its owning zone, collecting a per-host Result (a host with no
// owning zone yields a Result with Err set, but does not abort the others).
func (c *Client) SyncHosts(hosts []string, zoneID, target string, proxied bool, comment string, del, dryRun bool) ([]Result, error) {
	zoneID = strings.TrimSpace(zoneID)
	explicit := zoneID != ""
	var zones []Zone
	if !explicit {
		var err error
		if zones, err = c.ListZones(); err != nil {
			return nil, fmt.Errorf("list cloudflare zones: %w", err)
		}
	}
	out := make([]Result, 0, len(hosts))
	for _, h := range hosts {
		r := Result{Host: h}
		z := Zone{ID: zoneID, Name: "(explicit zone)"}
		if !explicit {
			var ok bool
			if z, ok = ZoneForHost(zones, h); !ok {
				r.Err = fmt.Errorf("no Cloudflare zone owns %q (zones visible to this token: %s); pass an explicit zone id", h, zoneNames(zones))
				out = append(out, r)
				continue
			}
		}
		r.Zone = z.Name
		if del {
			r.Action, r.Err = c.DeleteCNAME(z.ID, h, dryRun)
		} else {
			r.Action, r.Err = c.UpsertCNAME(z.ID, h, target, proxied, comment, dryRun)
		}
		out = append(out, r)
	}
	return out, nil
}

// TunnelIDFromToken decodes a cloudflared tunnel token — base64 JSON of the form
// {"a":<accountTag>,"t":<tunnelID>,"s":<secret>} — and returns the tunnel UUID
// (t). This lets the DNS step derive the CNAME target from the SAME TUNNEL_TOKEN
// the sidecar already uses, so no separate tunnel-id config is needed.
func TunnelIDFromToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("empty tunnel token")
	}
	raw, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		// Tokens may be unpadded or URL-safe base64.
		if raw, err = base64.RawStdEncoding.DecodeString(token); err != nil {
			if raw, err = base64.RawURLEncoding.DecodeString(token); err != nil {
				return "", fmt.Errorf("decode tunnel token (not base64): %w", err)
			}
		}
	}
	var tok struct {
		A string `json:"a"`
		T string `json:"t"`
		S string `json:"s"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return "", fmt.Errorf("parse tunnel token JSON: %w", err)
	}
	if tok.T == "" {
		return "", fmt.Errorf("tunnel token has no tunnel id (t)")
	}
	return tok.T, nil
}

// TunnelTarget is the CNAME content for a tunnel id: <tunnelID>.cfargotunnel.com.
func TunnelTarget(tunnelID string) string {
	return strings.TrimSpace(tunnelID) + "." + TunnelDomain
}
