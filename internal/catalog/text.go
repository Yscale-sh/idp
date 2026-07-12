package catalog

import (
	"fmt"
	"io"
)

// WriteText prints a compact terminal view of the catalog — the quick "what's in
// this env" glance, the visual HTML page's plain-text twin.
func (c *Catalog) WriteText(w io.Writer) {
	fmt.Fprintf(w, "env %s", c.Env)
	if c.Source != "" {
		fmt.Fprintf(w, "  (source: %s)", c.Source)
	}
	fmt.Fprintf(w, "  —  %d workload(s), %d module(s)\n", len(c.Apps), len(c.Modules))

	if len(c.Apps) > 0 {
		fmt.Fprintln(w, "\nWORKLOADS")
		for _, a := range c.Apps {
			fmt.Fprintf(w, "  %s\n", a.Workload)
			fmt.Fprintf(w, "    %-12s %s\n", "image", orDash(a.Image))
			fmt.Fprintf(w, "    %-12s %s\n", "namespace", a.Namespace)
			kind := fmt.Sprintf("%d replica(s)", a.Replicas)
			if a.Worker {
				kind = "worker (no Service) · " + kind
			} else {
				kind = fmt.Sprintf("port %d · %s", a.Port, kind)
			}
			if a.Autoscale != nil {
				kind += fmt.Sprintf(" · autoscale %d–%d", a.Autoscale.Min, a.Autoscale.Max)
			}
			fmt.Fprintf(w, "    %-12s %s\n", "runtime", kind)
			for _, r := range a.Routes {
				scope := "internal"
				if r.Public {
					scope = "public"
				}
				fmt.Fprintf(w, "    %-12s %s (%s)\n", "route", r.Host, scope)
			}
			if a.LAN != nil {
				fmt.Fprintf(w, "    %-12s %s\n", "lan", a.LAN.Display())
			}
			for _, d := range a.DBs {
				fmt.Fprintf(w, "    %-12s %s (%s)\n", "db", d.Name, d.Type)
			}
			for _, ch := range a.Caches {
				fmt.Fprintf(w, "    %-12s %s (%s)\n", "cache", ch.Name, ch.Type)
			}
			for _, s := range a.Stores {
				fmt.Fprintf(w, "    %-12s %s -> %s\n", "provisions", s.Tool, s.Namespace)
			}
			if a.Secret != nil {
				fmt.Fprintf(w, "    %-12s backend=%s key=%s\n", "secrets", a.Secret.Backend, orDash(a.Secret.Key))
			}
		}
	}

	if len(c.Modules) > 0 {
		fmt.Fprintln(w, "\nMODULES")
		for _, m := range c.Modules {
			ver := ""
			if m.Version != "" {
				ver = " " + m.Version
			}
			fmt.Fprintf(w, "  %-22s %s%s  -> %s\n", m.Name, m.Chart, ver, m.Namespace)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
