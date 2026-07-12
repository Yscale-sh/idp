package main

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
)

func TestTunnelFrontend(t *testing.T) {
	front := appconfig.App{App: "sample", Component: "router"}
	got, err := tunnelFrontend([]appconfig.App{front})
	if err != nil {
		t.Fatalf("single frontend: %v", err)
	}
	if got.Component != "router" {
		t.Fatalf("component = %q, want router", got.Component)
	}

	_, err = tunnelFrontend([]appconfig.App{
		front,
		{App: "sample", Component: "api"},
	})
	if err == nil || !strings.Contains(err.Error(), "one router/front component") {
		t.Fatalf("multiple frontends error = %v", err)
	}
}
