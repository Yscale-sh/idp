// Package catalog builds a read-only VIEW of an environment's committed desired
// state. It reads the rendered umbrella (clusters/<env>/platform.yaml) and
// projects it into a clean, typed model that the text/HTML/JSON renderers
// consume. It is strictly a projection: catalog never writes cluster state and
// never talks to a cluster. The single source of truth stays git — this is just
// a lens on it.
package catalog

import (
	"fmt"
	"os"
	"sort"

	"github.com/yscale-sh/idp/internal/render"
	"sigs.k8s.io/yaml"
)

// Catalog is the whole environment, projected for viewing.
type Catalog struct {
	Env     string   `json:"env"`
	Source  string   `json:"source,omitempty"` // the Flux GitRepository the umbrella pulls charts from
	Apps    []App    `json:"apps"`
	Modules []Module `json:"modules"`
}

// App is one workload (a single app, or one component of a multi-component app).
type App struct {
	Name      string      `json:"name"`
	Component string      `json:"component,omitempty"`
	Workload  string      `json:"workload"` // the umbrella key (<app>-<component> or <app>)
	Namespace string      `json:"namespace"`
	Image     string      `json:"image"`
	Worker    bool        `json:"worker"` // port == 0: Deployment only, no Service/probes
	Port      int         `json:"port,omitempty"`
	Replicas  int         `json:"replicas"`
	Autoscale *Autoscale  `json:"autoscale,omitempty"`
	Routes    []Route     `json:"routes,omitempty"`
	LAN       *LANExpose  `json:"lan,omitempty"`
	Stores    []Store     `json:"stores,omitempty"` // dedicated stores PROVISIONED for this workload
	DBs       []DataStore `json:"dbs,omitempty"`    // wired db URLs (provisioned or shared)
	Caches    []DataStore `json:"caches,omitempty"` // wired cache URLs (provisioned or shared)
	Secret    *Secret     `json:"secret,omitempty"`
}

// Autoscale is the KEDA scaling window, present only when autoscaling is enabled.
type Autoscale struct {
	Min  int    `json:"min"`
	Max  int    `json:"max"`
	Kind string `json:"kind,omitempty"` // HTTPScaledObject | ScaledObject
}

// Route is a public/internal hostname the workload serves.
type Route struct {
	Host         string `json:"host"`
	Public       bool   `json:"public"`
	Humans       bool   `json:"humans,omitempty"`       // Cloudflare Access for humans
	ServiceToken bool   `json:"serviceToken,omitempty"` // Cloudflare Access service token
}

// LANExpose is a MetalLB LoadBalancer on the LAN (the only sanctioned LB). An
// empty IP means the address is auto-assigned from a named IPAddressPool.
type LANExpose struct {
	IP   string `json:"ip,omitempty"`
	Port int    `json:"port,omitempty"`
}

// Display is the human-facing LAN address; an empty IP reads as pool-assigned.
func (l LANExpose) Display() string {
	ip := l.IP
	if ip == "" {
		ip = "pool-assigned"
	}
	if l.Port != 0 {
		return fmt.Sprintf("%s :%d", ip, l.Port)
	}
	return ip
}

// Store is a dedicated data store provisioned for the workload (its own HelmRelease).
type Store struct {
	Tool      string `json:"tool"` // postgres | redis | ...
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
}

// DataStore is a wired data-store connection (a db[] or cache[] entry): the
// platform injects its URL into the named env keys.
type DataStore struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	URLKeys []string `json:"urlKeys,omitempty"`
}

// Secret is where the workload's runtime secrets come from.
type Secret struct {
	Backend string `json:"backend"` // local | ssm | ...
	Key     string `json:"key,omitempty"`
}

// Module is one enabled platform module (a Flux HelmRelease).
type Module struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Source    string `json:"source"` // localChart | chartRepo
	Chart     string `json:"chart"`
	Version   string `json:"version,omitempty"`
	RepoURL   string `json:"repoURL,omitempty"`
}

// Load reads clusters/<env>/platform.yaml and builds the catalog. It errors if
// the file is absent — there is nothing to view until something is rendered.
func Load(root, env string) (*Catalog, error) {
	path := render.PlatformPath(root, env)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no rendered state for env %q (%s): run `idpctl render`/`idpctl infra render` first", env, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var pr render.PlatformRelease
	if err := yaml.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return Build(&pr), nil
}

// Build projects a parsed umbrella HelmRelease into the view model.
func Build(pr *render.PlatformRelease) *Catalog {
	cv := pr.Spec.Values
	c := &Catalog{Env: cv.Env, Source: cv.Source.Name}
	for _, e := range cv.Apps {
		c.Apps = append(c.Apps, buildApp(e))
	}
	for _, m := range cv.Modules {
		c.Modules = append(c.Modules, Module{
			Name: m.Name, Namespace: m.Namespace, Source: m.Source,
			Chart: m.Chart, Version: m.Version, RepoURL: m.RepoURL,
		})
	}
	sort.SliceStable(c.Apps, func(i, j int) bool { return c.Apps[i].Workload < c.Apps[j].Workload })
	sort.SliceStable(c.Modules, func(i, j int) bool { return c.Modules[i].Name < c.Modules[j].Name })
	return c
}

func buildApp(e render.AppEntry) App {
	v := asMap(e.Values)
	a := App{
		Name:      e.Name,
		Component: e.Component,
		Workload:  e.ReleaseName,
		Namespace: e.Namespace,
		Image:     image(v),
		Port:      toInt(dig(v, "port")),
		Replicas:  toInt(dig(v, "replicas")),
	}
	a.Worker = a.Port == 0

	if keda := asMap(dig(v, "keda")); toBool(keda["enabled"]) {
		a.Autoscale = &Autoscale{
			Min:  toInt(keda["minReplicas"]),
			Max:  toInt(keda["maxReplicas"]),
			Kind: toStr(keda["kind"]),
		}
	}

	for _, r := range asSlice(dig(v, "routes")) {
		rm := asMap(r)
		acc := asMap(rm["access"])
		a.Routes = append(a.Routes, Route{
			Host:         toStr(rm["host"]),
			Public:       toBool(rm["public"]),
			Humans:       toBool(acc["humans"]),
			ServiceToken: toBool(acc["serviceToken"]),
		})
	}

	if lan := asMap(dig(v, "lanExpose")); toBool(lan["enabled"]) {
		a.LAN = &LANExpose{IP: toStr(lan["ip"]), Port: toInt(lan["port"])}
	}

	a.DBs = dataStores(dig(v, "db"))
	a.Caches = dataStores(dig(v, "cache"))

	for _, s := range e.Stores {
		a.Stores = append(a.Stores, Store{Tool: s.Tool, Namespace: s.Namespace, Release: s.ReleaseName})
	}
	// Preserve the legacy single-Postgres shape in the view too.
	if e.Postgres != nil && e.Postgres.Enabled {
		a.Stores = append(a.Stores, Store{Tool: "postgres", Namespace: e.Postgres.Namespace, Release: e.Postgres.ReleaseName})
	}

	if es := asMap(dig(v, "externalSecret")); toBool(es["enabled"]) {
		a.Secret = &Secret{Backend: toStr(es["backend"]), Key: extractKey(es)}
	}
	return a
}

func dataStores(v any) []DataStore {
	var out []DataStore
	for _, d := range asSlice(v) {
		dm := asMap(d)
		ds := DataStore{Name: toStr(dm["name"]), Type: toStr(dm["type"])}
		for _, k := range asSlice(dm["urlKeys"]) {
			ds.URLKeys = append(ds.URLKeys, toStr(k))
		}
		out = append(out, ds)
	}
	return out
}

func image(v map[string]any) string {
	im := asMap(dig(v, "image"))
	repo, tag := toStr(im["repository"]), toStr(im["tag"])
	if repo == "" {
		return ""
	}
	if tag == "" {
		return repo
	}
	return repo + ":" + tag
}

// extractKey pulls the SSM/secret root from externalSecret.dataFrom[0].extract.key.
func extractKey(es map[string]any) string {
	for _, df := range asSlice(es["dataFrom"]) {
		if k := toStr(asMap(asMap(df)["extract"])["key"]); k != "" {
			return k
		}
	}
	return ""
}
