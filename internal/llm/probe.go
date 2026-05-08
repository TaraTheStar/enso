// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// probeInterval is how often the recovery probe pings the endpoint while
// the tracker is in StateDisconnected. Trades discovery latency against
// idle network noise; 5s feels recovery-aware without being chatty.
const probeInterval = 5 * time.Second

// probeTimeout caps each probe attempt. Short enough that a stuck DNS
// lookup or hung TCP connect doesn't pile up goroutines if we go through
// many cycles before recovery.
const probeTimeout = 3 * time.Second

// startRecoveryProbe spawns the at-most-one probe goroutine for c. Safe
// to call repeatedly — claimProbe drops duplicates. The goroutine exits
// once the endpoint answers (state flips to Connected) or the tracker
// has already been moved out of Disconnected by something else (e.g., a
// successful user-driven Chat call).
func (c *Client) startRecoveryProbe() {
	if !c.conn.claimProbe() {
		return
	}
	go c.probeLoop()
}

func (c *Client) probeLoop() {
	defer c.conn.releaseProbe()
	interval := probeInterval
	if c.ProbeInterval > 0 {
		interval = c.ProbeInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		if c.conn.get() != StateDisconnected {
			return
		}
		if c.probeOnce() {
			c.conn.set(StateConnected)
			return
		}
	}
}

// probeOnce returns true if the endpoint is reachable. Any HTTP response
// — including 4xx/5xx — counts as success: TLS+TCP completed, so the
// transport is healthy. Only outright transport failures keep the
// tracker disconnected.
func (c *Client) probeOnce() bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	url := strings.TrimSuffix(c.Endpoint, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
