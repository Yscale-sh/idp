package policy

import "strings"

// imageTag extracts the tag from a fully-qualified image reference. It handles a
// registry host that itself contains a ':' port (e.g.
// "registry:5000/app:tag"). Returns "" when no tag is present.
func imageTag(image string) string {
	image = strings.TrimSpace(image)
	if image == "" {
		return ""
	}
	// A digest ref ("...@sha256:...") is always immutable; treat as tagged.
	if i := strings.Index(image, "@"); i >= 0 {
		return image[i+1:]
	}
	// The tag, if any, is after the last ':' that comes after the last '/'.
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[lastColon+1:]
	}
	return ""
}
