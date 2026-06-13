package catalog

import (
	"encoding/json"
	"strconv"
)

// The umbrella values are read through sigs.k8s.io/yaml, which routes via JSON —
// so every scalar arrives as string/bool/float64 and every container as
// map[string]any / []any. These helpers read that shape defensively: a missing
// or wrong-typed field yields the zero value rather than panicking, because the
// rendered values evolve and a viewer must never crash on an unexpected map.

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// dig walks nested maps by key, returning nil if any hop is missing.
func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm := asMap(cur)
		if mm == nil {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	default:
		return ""
	}
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}
