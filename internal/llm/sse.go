// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bufio"
	"io"
	"strings"
)

// ParseSSE reads Server-Sent Events from an io.Reader and sends raw JSON
// payloads to the returned channel. The channel closes on [DONE] or EOF.
func ParseSSE(reader io.Reader, done chan []byte) {
	defer close(done)

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

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

		select {
		case done <- []byte(data):
		}
	}
}
