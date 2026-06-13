package appconfig

// Expand turns a multi-component App (App.Components non-empty) into one full App
// per component — each the shared base merged with that component's deltas — so a
// single shopping list authors a whole product. A plain single-component App (no
// Components) returns just itself, so every caller can uniformly
// `for _, a := range app.Expand()`. See Component for the merge rules.
//
// Stores are provisioned ONCE per app: the first component to use a given db/cache
// entry (by name) provisions it; any later component that shares it is auto-set
// provision:false. That reproduces the hand-written "api provisions, scanner/
// transcode provision:false" split with no per-component flags — and is applied on
// COPIES so the shared base slice is never mutated across components.
func (a App) Expand() []App {
	if len(a.Components) == 0 {
		return []App{a}
	}
	seenDB := map[string]bool{}
	seenCache := map[string]bool{}
	out := make([]App, 0, len(a.Components))
	for _, c := range a.Components {
		m := a // value copy of the shared base
		m.Components = nil
		m.Component = c.Component

		// Pointer fields: override the base only when the component sets them.
		if c.Runtime != nil {
			m.Runtime = *c.Runtime
		}
		if c.Port != nil {
			m.Runtime.Port = *c.Port // port-only override, keeps the (base or runtime) image
		}
		if c.Build != nil {
			m.Build = *c.Build
		}
		if c.Probes != nil {
			m.Probes = c.Probes
		}
		if c.Sizing != nil {
			m.Sizing = *c.Sizing
		}
		if c.Expose != nil {
			m.Expose = c.Expose
		}
		// Slice fields: inherit-or-replace. nil = inherit base; set (incl. []) = use
		// the component's, so `db: []` opts a component out of the app's stores.
		if c.Volumes != nil {
			m.Volumes = c.Volumes
		}
		if c.Routes != nil {
			m.Routes = c.Routes
		}
		if c.ConnectsTo != nil {
			m.ConnectsTo = c.ConnectsTo
		}
		if c.DB != nil {
			m.DB = c.DB
		}
		if c.Cache != nil {
			m.Cache = c.Cache
		}
		if c.Storage != nil {
			m.Storage = c.Storage
		}
		if c.Logging != nil {
			m.Logging = *c.Logging
		}
		if c.Metrics != nil {
			m.Metrics = *c.Metrics
		}
		// env merges (component wins per key); secrets union (deduped).
		m.Env = mergeEnv(a.Env, c.Env)
		m.Secrets = unionStrings(a.Secrets, c.Secrets)

		// App-level stores: first user provisions, the rest share.
		m.DB = provisionOnce(m.DB, seenDB)
		m.Cache = provisionOnce(m.Cache, seenCache)

		out = append(out, m)
	}
	return out
}

// mergeEnv overlays comp onto base (comp wins per key). nil when both empty so a
// component with no env renders identically to a single-file app with no env.
func mergeEnv(base, comp map[string]string) map[string]string {
	if len(base) == 0 && len(comp) == 0 {
		return nil
	}
	m := make(map[string]string, len(base)+len(comp))
	for k, v := range base {
		m[k] = v
	}
	for k, v := range comp {
		m[k] = v
	}
	return m
}

// unionStrings concatenates base+extra, order-stable and de-duplicated. nil when
// both empty.
func unionStrings(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, s := range append(append([]string{}, base...), extra...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// provisionOnce returns a COPY of stores in which the first occurrence of each
// store name (across the whole app, tracked in seen) provisions and every later
// occurrence is set provision:false. Copying keeps Expand from mutating the base.
func provisionOnce(stores []DataStore, seen map[string]bool) []DataStore {
	if stores == nil {
		return nil
	}
	out := make([]DataStore, len(stores))
	for i, s := range stores {
		if seen[s.Name] {
			no := false
			s.Provision = &no // share the sibling component's store
		} else {
			seen[s.Name] = true // first user provisions (Provision left as authored)
		}
		out[i] = s
	}
	return out
}
