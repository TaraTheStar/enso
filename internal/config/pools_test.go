// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"testing"
	"time"
)

func TestResolvePools_AutoGroupBySharedEndpoint(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"fast": {Endpoint: "http://latchkey:4000/v1", Model: "a"},
		"deep": {Endpoint: "http://latchkey:4000/v1", Model: "b"},
		"halo": {Endpoint: "http://halo:8080/v1", Model: "c"},
	}}
	r := c.ResolvePools()

	if r.Assignment["fast"] != r.Assignment["deep"] {
		t.Errorf("same-endpoint providers should share a pool: %q vs %q",
			r.Assignment["fast"], r.Assignment["deep"])
	}
	if r.Assignment["fast"] == r.Assignment["halo"] {
		t.Errorf("distinct endpoints must not co-pool: both %q", r.Assignment["fast"])
	}
	if name := r.Assignment["fast"]; name != "auto-latchkey-4000" {
		t.Errorf("auto pool name = %q, want auto-latchkey-4000", name)
	}
	// Multi-member auto pool defaults to serialised concurrency 1.
	if cc := r.Pools[r.Assignment["fast"]].Concurrency; cc != 1 {
		t.Errorf("shared auto pool concurrency = %d, want 1", cc)
	}
	if to := r.Pools[r.Assignment["fast"]].QueueTimeout; to != DefaultQueueTimeout {
		t.Errorf("queue timeout = %v, want default %v", to, DefaultQueueTimeout)
	}
}

func TestResolvePools_LonePoolInheritsProviderConcurrency(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"solo": {Endpoint: "http://a:1/v1", Model: "m", Concurrency: 4},
	}}
	r := c.ResolvePools()
	if cc := r.Pools[r.Assignment["solo"]].Concurrency; cc != 4 {
		t.Errorf("lone pool concurrency = %d, want provider's 4", cc)
	}
}

func TestResolvePools_ExplicitOverrideAndBlock(t *testing.T) {
	c := &Config{
		Providers: map[string]ProviderConfig{
			// Same endpoint but pinned to different explicit pools.
			"a": {Endpoint: "http://shared:1/v1", Pool: "gpu"},
			"b": {Endpoint: "http://shared:1/v1", Pool: "gpu"},
			"c": {Endpoint: "http://shared:1/v1"},
		},
		Pools: map[string]PoolConfig{
			"gpu": {Concurrency: 2, QueueTimeout: "10s", RPM: 500},
		},
	}
	r := c.ResolvePools()

	if r.Assignment["a"] != "gpu" || r.Assignment["b"] != "gpu" {
		t.Errorf("explicit pool= override not applied: a=%q b=%q",
			r.Assignment["a"], r.Assignment["b"])
	}
	if r.Assignment["c"] == "gpu" {
		t.Errorf("provider without pool= should not join explicit gpu pool")
	}
	gpu := r.Pools["gpu"]
	if gpu.Concurrency != 2 {
		t.Errorf("gpu concurrency = %d, want 2", gpu.Concurrency)
	}
	if gpu.QueueTimeout != 10*time.Second {
		t.Errorf("gpu queue timeout = %v, want 10s", gpu.QueueTimeout)
	}
}

func TestResolvePools_InvalidTimeoutFallsBackToDefault(t *testing.T) {
	c := &Config{
		Providers: map[string]ProviderConfig{"a": {Endpoint: "http://h:1/v1", Pool: "p"}},
		Pools:     map[string]PoolConfig{"p": {QueueTimeout: "not-a-duration"}},
	}
	r := c.ResolvePools()
	if to := r.Pools["p"].QueueTimeout; to != DefaultQueueTimeout {
		t.Errorf("invalid duration should fall back to default, got %v", to)
	}
}

// A [pools.X] block that tunes only queue_timeout must NOT clamp a lone
// provider's own concurrency back to 1 — the block overrides concurrency
// only when it sets it.
func TestResolvePools_BlockWithoutConcurrencyKeepsProviderConcurrency(t *testing.T) {
	c := &Config{
		Providers: map[string]ProviderConfig{
			"solo": {Endpoint: "http://a:1/v1", Pool: "p", Concurrency: 8},
		},
		Pools: map[string]PoolConfig{
			"p": {QueueTimeout: "60s"}, // concurrency unset
		},
	}
	r := c.ResolvePools()
	if cc := r.Pools["p"].Concurrency; cc != 8 {
		t.Errorf("block without concurrency clamped provider's 8 to %d", cc)
	}
	if to := r.Pools["p"].QueueTimeout; to != 60*time.Second {
		t.Errorf("queue timeout = %v, want 60s", to)
	}
}

// Scheme-less endpoints ("host:port", no http://) must still auto-group:
// url.Parse puts the host in Scheme with an empty Host, so autoPoolName
// retries as a network reference.
func TestResolvePools_SchemelessEndpointsCoPool(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"a": {Endpoint: "localhost:8080", Model: "m"},
		"b": {Endpoint: "localhost:8080", Model: "n"},
		"c": {Endpoint: "localhost:9090", Model: "o"},
	}}
	r := c.ResolvePools()
	if r.Assignment["a"] != r.Assignment["b"] {
		t.Errorf("scheme-less same endpoint should co-pool: %q vs %q",
			r.Assignment["a"], r.Assignment["b"])
	}
	if r.Assignment["a"] == r.Assignment["c"] {
		t.Errorf("scheme-less distinct ports must not co-pool: both %q", r.Assignment["a"])
	}
	if name := r.Assignment["a"]; name != "auto-localhost-8080" {
		t.Errorf("auto pool name = %q, want auto-localhost-8080", name)
	}
}

func TestResolvePools_UnparseableEndpointGetsOwnPool(t *testing.T) {
	c := &Config{Providers: map[string]ProviderConfig{
		"x": {Endpoint: "::not a url::", Model: "m"},
		"y": {Endpoint: "://also bad", Model: "m"},
	}}
	r := c.ResolvePools()
	if r.Assignment["x"] == r.Assignment["y"] {
		t.Errorf("unparseable endpoints must not silently co-pool: both %q", r.Assignment["x"])
	}
	if r.Assignment["x"] != "auto-x" {
		t.Errorf("fallback pool = %q, want auto-x", r.Assignment["x"])
	}
}
