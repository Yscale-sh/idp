package policy

import "bytes"

// splitYAMLDocs splits a multi-document YAML stream on "---" separators.
func splitYAMLDocs(data []byte) [][]byte {
	var docs [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		part = bytes.TrimSpace(part)
		part = bytes.TrimPrefix(part, []byte("---"))
		part = bytes.TrimSpace(part)
		if len(part) > 0 {
			docs = append(docs, part)
		}
	}
	return docs
}

// kindOf returns the Kubernetes Kind of a decoded object.
func kindOf(obj map[string]any) string {
	if k, ok := obj["kind"].(string); ok {
		return k
	}
	return ""
}

// metaName returns metadata.name of a decoded object.
func metaName(obj map[string]any) string {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if n, ok := meta["name"].(string); ok {
			return n
		}
	}
	return ""
}
