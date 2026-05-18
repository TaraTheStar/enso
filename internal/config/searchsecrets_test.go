// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSearchSecrets_ReadsCACertIntoPEM(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	want := []byte("-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n")
	if err := os.WriteFile(p, want, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := &Config{}
	c.Search.SearXNG.CACert = p

	var warn bytes.Buffer
	c.ResolveSearchSecrets(&warn)
	if !bytes.Equal(c.Search.SearXNG.CACertPEM, want) {
		t.Fatalf("CACertPEM = %q, want %q", c.Search.SearXNG.CACertPEM, want)
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warning on success: %q", warn.String())
	}

	// Idempotent: a second call must not re-read or warn.
	c.ResolveSearchSecrets(&warn)
	if warn.Len() != 0 {
		t.Errorf("second call warned: %q", warn.String())
	}
}

func TestResolveSearchSecrets_MissingFileWarnsAndFallsBack(t *testing.T) {
	c := &Config{}
	c.Search.SearXNG.CACert = "/no/such/ca.pem"

	var warn bytes.Buffer
	c.ResolveSearchSecrets(&warn)

	if len(c.Search.SearXNG.CACertPEM) != 0 {
		t.Errorf("CACertPEM should stay empty on read failure, got %q", c.Search.SearXNG.CACertPEM)
	}
	w := warn.String()
	if !strings.Contains(w, "/no/such/ca.pem") || !strings.Contains(w, "default TLS trust") {
		t.Errorf("warning must name the path and the fallback, got %q", w)
	}
}

func TestResolveSearchSecrets_NoConfigIsNoop(t *testing.T) {
	c := &Config{}
	var warn bytes.Buffer
	c.ResolveSearchSecrets(&warn)
	if warn.Len() != 0 || len(c.Search.SearXNG.CACertPEM) != 0 {
		t.Errorf("no ca_cert configured must be a silent no-op")
	}
}

// The resolved bytes must cross the worker seam: ScrubbedForWorker
// JSON-round-trips the config, and CACertPEM has no worker:"deny" tag,
// so a sealed worker (no host config dir mounted) still trusts the CA.
func TestResolveSearchSecrets_PEMSurvivesScrub(t *testing.T) {
	c := &Config{}
	c.Search.SearXNG.CACert = "/host/only/ca.pem"
	c.Search.SearXNG.CACertPEM = []byte("PEMDATA")

	got := c.ScrubbedForWorker()
	if string(got.Search.SearXNG.CACertPEM) != "PEMDATA" {
		t.Fatalf("CACertPEM did not survive ScrubbedForWorker: %q", got.Search.SearXNG.CACertPEM)
	}
}
