package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"browsercompute.local/runtime/internal/identity"
)

func TestAlternateHelp(t *testing.T) {
	var out bytes.Buffer
	d := identity.Display{Name: "Alternate", CLI: "alternate", Tagline: "Tag", Description: "Description"}
	if err := run(context.Background(), []string{"--help"}, &out, &out, d); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{d.Name, d.CLI, d.Tagline, d.Description} {
		if !strings.Contains(out.String(), part) {
			t.Fatalf("missing %q in %q", part, out.String())
		}
	}
}
