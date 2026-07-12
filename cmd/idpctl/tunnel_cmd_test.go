package main

import "testing"

// TestIsAccessProtected verifies the pure classification function that decides
// whether a (status, location) pair from an unauthenticated GET indicates
// Cloudflare Access protection. No network — table-driven, no side effects.
func TestIsAccessProtected(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		want     bool
	}{
		{
			name:     "302 with cloudflareaccess.com location ok",
			status:   302,
			location: "https://litewindow.cloudflareaccess.com/cdn-cgi/access/login/litewindow-dev.yscale.sh",
			want:     true,
		},
		{
			name:     "301 with cloudflareaccess.com location ok",
			status:   301,
			location: "https://myapp.cloudflareaccess.com/",
			want:     true,
		},
		{
			name:     "303 with cloudflareaccess.com location ok",
			status:   303,
			location: "https://baz.cloudflareaccess.com/",
			want:     true,
		},
		{
			name:     "307 with cloudflareaccess.com location ok",
			status:   307,
			location: "https://foo.cloudflareaccess.com/auth",
			want:     true,
		},
		{
			name:     "308 with cloudflareaccess.com location ok",
			status:   308,
			location: "https://bar.cloudflareaccess.com/",
			want:     true,
		},
		{
			name:     "200 with cloudflareaccess.com location not ok — not a redirect",
			status:   200,
			location: "https://litewindow.cloudflareaccess.com/",
			want:     false,
		},
		{
			name:     "302 to unrelated host not ok",
			status:   302,
			location: "https://example.com/login",
			want:     false,
		},
		{
			name:     "302 with empty location not ok",
			status:   302,
			location: "",
			want:     false,
		},
		{
			name:     "404 with cloudflareaccess.com location not ok — not a redirect",
			status:   404,
			location: "https://x.cloudflareaccess.com/",
			want:     false,
		},
		{
			name:     "302 to site that contains cloudflareaccess.com as substring ok",
			status:   302,
			location: "https://sub.cloudflareaccess.com/path?foo=bar",
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAccessProtected(tc.status, tc.location); got != tc.want {
				t.Fatalf("isAccessProtected(%d, %q) = %v, want %v", tc.status, tc.location, got, tc.want)
			}
		})
	}
}
