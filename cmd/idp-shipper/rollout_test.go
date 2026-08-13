package main

import (
	"testing"

	"github.com/yscale-sh/idp/internal/render"
)

// TestRolloutTargetsIncludeBucketReleases pins the rule that a ship's health is
// not just its workloads: a provisioned bucket is part of what the ship
// delivers, so its release belongs in the target set. It is marked
// workload-free because a bucket release owns no Deployment and carries no
// DEPLOY_TIME — its readiness is the Helm hook Job completing.
func TestRolloutTargetsIncludeBucketReleases(t *testing.T) {
	entries := []render.AppEntry{
		{
			Name: "media", ReleaseName: "media-api", Namespace: "media-dev",
			Buckets: []render.BucketEntry{{
				ReleaseName: "media-dev-uploads-bucket", Namespace: "platform-storage",
			}},
		},
		// The sibling component shares the bucket, so it renders no bucket
		// release of its own and must not duplicate the target.
		{Name: "media", ReleaseName: "media-worker", Namespace: "media-dev"},
	}

	got := rolloutTargets(entries)
	want := []rolloutTarget{
		{ReleaseName: "media-api", Namespace: "media-dev", Kind: "app", RequiresWorkload: true},
		{ReleaseName: "media-dev-uploads-bucket", Namespace: "platform-storage", Kind: "bucket", RequiresWorkload: false},
		{ReleaseName: "media-worker", Namespace: "media-dev", Kind: "app", RequiresWorkload: true},
	}
	if len(got) != len(want) {
		t.Fatalf("rolloutTargets() = %+v, want %d targets", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("target %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRolloutTargetsDedupeSharedBucketRelease(t *testing.T) {
	bucket := render.BucketEntry{ReleaseName: "media-dev-uploads-bucket", Namespace: "platform-storage"}
	entries := []render.AppEntry{
		{Name: "media", ReleaseName: "media-api", Namespace: "media-dev", Buckets: []render.BucketEntry{bucket}},
		{Name: "media", ReleaseName: "media-worker", Namespace: "media-dev", Buckets: []render.BucketEntry{bucket}},
	}
	got := rolloutTargets(entries)
	buckets := 0
	for _, target := range got {
		if target.Kind == "bucket" {
			buckets++
		}
	}
	if buckets != 1 {
		t.Fatalf("shared bucket produced %d targets, want 1: %+v", buckets, got)
	}
}

func TestRolloutTargetsSkipEmptyReleases(t *testing.T) {
	got := rolloutTargets([]render.AppEntry{{Name: "media"}})
	if len(got) != 0 {
		t.Fatalf("entry with no release name produced targets: %+v", got)
	}
}
