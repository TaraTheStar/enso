// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bufio"
	"bytes"
	"io"
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

	dataPrefix := []byte("data: ")
	doneMarker := []byte("[DONE]")

	for scanner.Scan() {
		// Work on the scanner's buffer directly: scanner.Text() would
		// copy every line into a string and the channel send would copy
		// it again. One copy of the payload is unavoidable (the scanner
		// reuses its buffer on the next Scan), but only one.
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if line[0] == ':' {
			continue
		}
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}

		data := line[len(dataPrefix):]
		if bytes.Equal(data, doneMarker) {
			return
		}

		out := make([]byte, len(data))
		copy(out, data)
		done <- out
	}

	if err := scanner.Err(); err != nil && errOut != nil {
		*errOut = err
	}
}
