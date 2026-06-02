// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bufio"
	"io"
	"strings"
)

// maxSSELineBytes caps a single SSE line. Tool-call argument blobs and
// large content deltas can be sizeable, so the ceiling is generous; a line
// past it is a genuine error (truncated/garbage stream), surfaced via the
// scanner error rather than silently swallowed.
const maxSSELineBytes = 16 * 1024 * 1024

// ParseSSE reads Server-Sent Events from an io.Reader and sends raw JSON
// payloads to the done channel. The channel closes on [DONE], EOF, or a
// read/scan error.
//
// If errOut is non-nil, the terminal scanner error (read failure, or a line
// exceeding maxSSELineBytes — bufio.ErrTooLong) is stored there before the
// channel closes. Closing done establishes a happens-before edge, so a
// receiver that observes the closed channel may safely read *errOut. A nil
// or unset *errOut means the stream ended cleanly (EOF or [DONE]).
//
// Without this, a truncated or oversized stream looked identical to a clean
// finish: the caller saw the channel close, emitted EventDone with an empty
// finish reason, and persisted a partial assistant message as if final.
func ParseSSE(reader io.Reader, done chan []byte, errOut *error) {
	defer close(done)

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 256*1024), maxSSELineBytes)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return
		}

		done <- []byte(data)
	}

	if err := scanner.Err(); err != nil && errOut != nil {
		*errOut = err
	}
}
