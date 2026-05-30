// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"testing"
	"time"
)

func TestBashConfig_ResolveTimeout(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultBashCommandTimeout},
		{"45s", 45 * time.Second},
		{"2m", 2 * time.Minute},
		{"0s", DisabledTimeout}, // explicit opt-out
		{"garbage", DefaultBashCommandTimeout},
		{"-5s", DefaultBashCommandTimeout}, // negative is nonsense → default
	}
	for _, tc := range cases {
		if got := (BashConfig{CommandTimeout: tc.in}).ResolveTimeout(); got != tc.want {
			t.Errorf("ResolveTimeout(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestBashConfig_ResolveTimeoutMax(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultBashCommandTimeoutMax}, // unset → default 1h ceiling
		{"10m", 10 * time.Minute},
		{"2h", 2 * time.Hour},
		{"0s", DefaultBashCommandTimeoutMax}, // a zero ceiling makes no sense → default
		{"bad", DefaultBashCommandTimeoutMax},
	}
	for _, tc := range cases {
		if got := (BashConfig{CommandTimeoutMax: tc.in}).ResolveTimeoutMax(); got != tc.want {
			t.Errorf("ResolveTimeoutMax(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestMCPConfig_ResolveCallTimeout(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultMCPCallTimeout},
		{"30s", 30 * time.Second},
		{"0s", 0}, // explicit disable
		{"bad", DefaultMCPCallTimeout},
	}
	for _, tc := range cases {
		if got := (MCPConfig{CallTimeout: tc.in}).ResolveCallTimeout(); got != tc.want {
			t.Errorf("ResolveCallTimeout(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
