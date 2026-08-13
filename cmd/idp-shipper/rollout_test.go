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

func TestReleaseIsReadyRequiresObservedReadyCondition(t *testing.T) {
	var release releaseReadiness
	target := rolloutTarget{RequiresWorkload: true, DeployTime: "2026-08-13T12:00:00Z"}
	release.Metadata.Generation = 3
	release.Status.ObservedGeneration = 2
	release.Status.Conditions = []releaseCondition{{Type: "Ready", Status: "True"}}
	release.Spec.Values.Env.TierA = map[string]string{"DEPLOY_TIME": target.DeployTime}
	if releaseIsReady(release, target) {
		t.Fatal("stale observedGeneration reported ready")
	}
	release.Status.ObservedGeneration = 3
	if !releaseIsReady(release, target) {
		t.Fatal("observed Ready=True release reported unready")
	}
	release.Spec.Values.Env.TierA["DEPLOY_TIME"] = "old"
	if releaseIsReady(release, target) {
		t.Fatal("previous ship stamp reported ready")
	}
	release.Spec.Values.Env.TierA["DEPLOY_TIME"] = target.DeployTime
	release.Status.Conditions[0].Status = "False"
	if releaseIsReady(release, target) {
		t.Fatal("Ready=False release reported ready")
	}
	release.Status.Conditions[0].Status = "True"
	if !releaseIsReady(release, rolloutTarget{Kind: "bucket"}) {
		t.Fatal("ready bucket release incorrectly required an app deploy stamp")
	}
}
