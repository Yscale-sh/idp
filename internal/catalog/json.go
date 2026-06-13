package catalog

import "encoding/json"

// JSON renders the catalog as indented JSON — the machine-readable view (feed a
// dashboard, diff two envs, drive a Pages build).
func JSON(c *Catalog) ([]byte, error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
