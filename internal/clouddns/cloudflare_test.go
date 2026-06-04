package clouddns

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTunnelIDFromToken(t *testing.T) {
	id := "7b3e1f2a-0000-4444-8888-aabbccddeeff"
	payload, _ := json.Marshal(map[string]string{"a": "acctag", "t": id, "s": "supersecret"})

	cases := map[string]string{
		"std-padded":  base64.StdEncoding.EncodeToString(payload),
		"raw-std":     base64.RawStdEncoding.EncodeToString(payload),
		"raw-urlsafe": base64.RawURLEncoding.EncodeToString(payload),
	}
	for name, token := range cases {
		got, err := TunnelIDFromToken(token)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if got != id {
			t.Errorf("%s: tunnel id = %q, want %q", name, got, id)
		}
	}

	if _, err := TunnelIDFromToken(""); err == nil {
		t.Error("empty token: expected error, got nil")
	}
	if _, err := TunnelIDFromToken("not-base64-!!!"); err == nil {
		t.Error("garbage token: expected error, got nil")
	}
	// Valid base64 but no tunnel id.
	noID := base64.StdEncoding.EncodeToString([]byte(`{"a":"x","s":"y"}`))
	if _, err := TunnelIDFromToken(noID); err == nil {
		t.Error("token without t: expected error, got nil")
	}
}

func TestTunnelTarget(t *testing.T) {
	if got := TunnelTarget("abc-123"); got != "abc-123.cfargotunnel.com" {
		t.Errorf("TunnelTarget = %q", got)
	}
}

func TestZoneForHost(t *testing.T) {
	zones := []Zone{
		{ID: "z1", Name: "example.com"},
		{ID: "z2", Name: "sub.example.com"}, // more specific delegated zone
		{ID: "z3", Name: "other.org"},
	}
	tests := []struct {
		host   string
		wantID string
		wantOK bool
	}{
		{"app.example.com", "z1", true},
		{"api.sub.example.com", "z2", true},       // longest suffix wins
		{"sub.example.com", "z2", true},           // exact match on delegated zone
		{"example.com", "z1", true},               // apex
		{"thing.other.org", "z3", true},
		{"nope.elsewhere.net", "", false},
	}
	for _, tt := range tests {
		z, ok := ZoneForHost(zones, tt.host)
		if ok != tt.wantOK || z.ID != tt.wantID {
			t.Errorf("ZoneForHost(%q) = (%q,%v), want (%q,%v)", tt.host, z.ID, ok, tt.wantID, tt.wantOK)
		}
	}
}

// fakeCF is a minimal in-memory Cloudflare API for the sync/prune tests.
type fakeCF struct {
	t         *testing.T
	records   map[string]Record // keyed by record name
	posts     int
	puts      int
	deletes   int
	zoneLists int
}

func (f *fakeCF) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/zones", func(w http.ResponseWriter, r *http.Request) {
		f.zoneLists++
		writeOK(w, []Zone{{ID: "zone1", Name: "example.com"}}, &resultInfo{Page: 1, TotalPages: 1})
	})
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			name := strings.ToLower(r.URL.Query().Get("name"))
			var out []Record
			if rec, ok := f.records[name]; ok {
				out = append(out, rec)
			}
			writeOK(w, out, nil)
		case http.MethodPost:
			f.posts++
			var rec Record
			_ = json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = "rec-" + rec.Name
			f.records[strings.ToLower(rec.Name)] = rec
			writeOK(w, rec, nil)
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	// Update / delete by record id.
	mux.HandleFunc("/zones/zone1/dns_records/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			f.puts++
			var rec Record
			_ = json.NewDecoder(r.Body).Decode(&rec)
			f.records[strings.ToLower(rec.Name)] = rec
			writeOK(w, rec, nil)
		case http.MethodDelete:
			f.deletes++
			for k, rec := range f.records {
				if "/zones/zone1/dns_records/"+rec.ID == r.URL.Path {
					delete(f.records, k)
				}
			}
			writeOK(w, map[string]string{"id": "deleted"}, nil)
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func writeOK(w http.ResponseWriter, result any, info *resultInfo) {
	raw, _ := json.Marshal(result)
	resp := apiResponse{Success: true, Result: raw, ResultInfo: info}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newTestClient(t *testing.T) (*Client, *fakeCF) {
	f := &fakeCF{t: t, records: map[string]Record{}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New("test-token")
	c.BaseURL = srv.URL
	return c, f
}

func TestSyncHosts_CreateUpdateUnchanged(t *testing.T) {
	c, f := newTestClient(t)
	target := "tunnel-uuid.cfargotunnel.com"
	hosts := []string{"app.example.com"}

	// 1. First sync creates the record.
	res, err := c.SyncHosts(hosts, "", target, true, "c", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != ActionCreated || res[0].Zone != "example.com" || res[0].Err != nil {
		t.Fatalf("create: got %+v", res[0])
	}
	if f.posts != 1 {
		t.Fatalf("expected 1 POST, got %d", f.posts)
	}

	// 2. Re-sync with identical desired state is unchanged (no write).
	res, _ = c.SyncHosts(hosts, "", target, true, "c", false, false)
	if res[0].Action != ActionUnchanged {
		t.Fatalf("re-sync: want unchanged, got %s", res[0].Action)
	}
	if f.posts != 1 || f.puts != 0 {
		t.Fatalf("unchanged should not write (posts=%d puts=%d)", f.posts, f.puts)
	}

	// 3. Changing the target updates the record.
	res, _ = c.SyncHosts(hosts, "", "new-tunnel.cfargotunnel.com", true, "c", false, false)
	if res[0].Action != ActionUpdated {
		t.Fatalf("changed target: want updated, got %s", res[0].Action)
	}
	if f.puts != 1 {
		t.Fatalf("expected 1 PUT, got %d", f.puts)
	}
}

func TestSyncHosts_DryRunNoWrites(t *testing.T) {
	c, f := newTestClient(t)
	res, err := c.SyncHosts([]string{"app.example.com"}, "", "t.cfargotunnel.com", true, "c", false, true /* dryRun */)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != ActionCreated {
		t.Fatalf("dry-run create: want created, got %s", res[0].Action)
	}
	if f.posts != 0 || f.puts != 0 || f.deletes != 0 {
		t.Fatalf("dry-run must not write (posts=%d puts=%d deletes=%d)", f.posts, f.puts, f.deletes)
	}
}

func TestSyncHosts_Prune(t *testing.T) {
	c, f := newTestClient(t)
	hosts := []string{"app.example.com"}
	// Create then prune.
	if _, err := c.SyncHosts(hosts, "", "t.cfargotunnel.com", true, "c", false, false); err != nil {
		t.Fatal(err)
	}
	res, err := c.SyncHosts(hosts, "", "", true, "c", true /* del */, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != ActionDeleted {
		t.Fatalf("prune: want deleted, got %s", res[0].Action)
	}
	if f.deletes != 1 {
		t.Fatalf("expected 1 DELETE, got %d", f.deletes)
	}
	// Pruning again is absent (idempotent).
	res, _ = c.SyncHosts(hosts, "", "", true, "c", true, false)
	if res[0].Action != ActionAbsent {
		t.Fatalf("re-prune: want absent, got %s", res[0].Action)
	}
}

func TestSyncHosts_NoZone(t *testing.T) {
	c, _ := newTestClient(t)
	res, err := c.SyncHosts([]string{"app.elsewhere.net"}, "", "t.cfargotunnel.com", true, "c", false, false)
	if err != nil {
		t.Fatalf("SyncHosts itself should not error on an unowned host: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("expected per-host error for a host with no owning zone")
	}
}

func TestSyncHosts_ExplicitZone(t *testing.T) {
	c, f := newTestClient(t)
	// An explicit zone id writes into THAT zone with no ListZones call — so it
	// works even for a host the token can't resolve a zone for (single-zone token).
	res, err := c.SyncHosts([]string{"anything.elsewhere.net"}, "zone1", "t.cfargotunnel.com", true, "c", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Err != nil || res[0].Action != ActionCreated {
		t.Fatalf("explicit zone create: got %+v", res[0])
	}
	if f.zoneLists != 0 {
		t.Fatalf("explicit zone must NOT list zones, got %d ListZones calls", f.zoneLists)
	}
	if f.posts != 1 {
		t.Fatalf("expected 1 POST, got %d", f.posts)
	}
}
