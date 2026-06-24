package policy

import (
	"strconv"
	"strings"
)

// ResourceSpec is one requests/limits pair as Kubernetes quantity strings.
type ResourceSpec struct {
	CPU    string
	Memory string
}

// ResourceEnvelope is the requests + limits the renderer copies into values
// (resources.requests / resources.limits).
type ResourceEnvelope struct {
	Requests ResourceSpec
	Limits   ResourceSpec
}

// profileResources mirrors charts/app/values.yaml resourceProfiles. The renderer
// and policy share this single source so a profile means the same thing in both.
var profileResources = map[string]ResourceEnvelope{
	"minimal": {
		Requests: ResourceSpec{CPU: "50m", Memory: "64Mi"},
		Limits:   ResourceSpec{CPU: "250m", Memory: "256Mi"},
	},
	"small": {
		Requests: ResourceSpec{CPU: "100m", Memory: "128Mi"},
		Limits:   ResourceSpec{CPU: "500m", Memory: "512Mi"},
	},
	"medium": {
		Requests: ResourceSpec{CPU: "250m", Memory: "256Mi"},
		Limits:   ResourceSpec{CPU: "1", Memory: "1Gi"},
	},
	"large": {
		Requests: ResourceSpec{CPU: "500m", Memory: "512Mi"},
		Limits:   ResourceSpec{CPU: "2", Memory: "2Gi"},
	},
	// Heavy, bursty workloads (e.g. in-process media transcode): a generous
	// ceiling so a 4K job doesn't OOM mid-stream. Pair with sizing.autosize (VPA)
	// so the actual pod is right-sized within this envelope.
	"xlarge": {
		Requests: ResourceSpec{CPU: "1", Memory: "1Gi"},
		Limits:   ResourceSpec{CPU: "4", Memory: "4Gi"},
	},
	// Whole-node dev workloads — the litewindow cockpit runs cargo builds + multiple
	// LLM agents + chromium + cartogopher in-pod and OOMs xlarge's 4Gi cap. Requests
	// stay tiny so it always schedules, incl. the always-on optiplex fallback (~14Gi)
	// when the r730 drains nightly. The 16Gi limit is a CEILING the pod only fully
	// realizes on a >=16Gi node (node4/5/6); on optiplex it bursts to ~node capacity.
	// NOTE: a limit above a node's RAM means node-level OOM (can hit neighbors), not a
	// contained cgroup kill — size the always-on node accordingly. ("allow huge sizing")
	"huge": {
		Requests: ResourceSpec{CPU: "1", Memory: "2Gi"},
		Limits:   ResourceSpec{CPU: "8", Memory: "16Gi"},
	},
}

// ProfileResources returns the resource envelope for a profile.
func ProfileResources(profile string) (ResourceEnvelope, bool) {
	r, ok := profileResources[profile]
	return r, ok
}

// parseCPU converts a Kubernetes CPU quantity to millicores. "250m" -> 250,
// "1" -> 1000, "2" -> 2000. Returns -1 on parse failure.
func parseCPU(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return -1
	}
	if strings.HasSuffix(q, "m") {
		n, err := strconv.ParseInt(strings.TrimSuffix(q, "m"), 10, 64)
		if err != nil {
			return -1
		}
		return n
	}
	f, err := strconv.ParseFloat(q, 64)
	if err != nil {
		return -1
	}
	return int64(f * 1000)
}

// memSuffixes maps Kubernetes binary/decimal memory suffixes to byte multipliers.
var memSuffixes = []struct {
	suffix string
	mult   int64
}{
	{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
	{"G", 1e9}, {"M", 1e6}, {"K", 1e3},
}

// parseMemory converts a Kubernetes memory quantity to bytes. Returns -1 on
// parse failure.
func parseMemory(q string) int64 {
	q = strings.TrimSpace(q)
	if q == "" {
		return -1
	}
	for _, s := range memSuffixes {
		if strings.HasSuffix(q, s.suffix) {
			n, err := strconv.ParseInt(strings.TrimSuffix(q, s.suffix), 10, 64)
			if err != nil {
				return -1
			}
			return n * s.mult
		}
	}
	n, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func exceedsCPU(have, max string) bool {
	h, m := parseCPU(have), parseCPU(max)
	if h < 0 || m < 0 {
		return false
	}
	return h > m
}

func exceedsMemory(have, max string) bool {
	h, m := parseMemory(have), parseMemory(max)
	if h < 0 || m < 0 {
		return false
	}
	return h > m
}
