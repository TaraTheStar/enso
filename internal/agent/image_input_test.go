// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

// TestRun_UserInputCarriesImageParts: a UserInput with image Parts lands
// as a multimodal user message — kept on History AND sent to the provider
// — so a vision model sees an attached image without a tool round-trip.
func TestRun_UserInputCarriesImageParts(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "I see a red square."})

	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        4,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan UserInput, 1)
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx, inputCh); close(runDone) }()

	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	inputCh <- UserInput{
		Text:  "what's in @shot.png?",
		Parts: []llm.MessagePart{llm.NewImagePart("image/png", png)},
	}

	// Wait until the provider has been called (the turn ran), then stop
	// the agent and JOIN its goroutine before reading History — History
	// is mutated by the Run goroutine, so reading it concurrently races.
	deadline := time.After(2 * time.Second)
	for mock.CallCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("provider never called")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	// 1) History carries the multimodal user message.
	var userMsg *llm.Message
	for i := range a.History {
		if a.History[i].Role == "user" {
			userMsg = &a.History[i]
			break
		}
	}
	if userMsg == nil {
		t.Fatal("no user message on History")
	}
	if len(userMsg.Parts) != 1 || userMsg.Parts[0].Type != "image" {
		t.Fatalf("user message lost its image part: %+v", userMsg.Parts)
	}

	// 2) The image reached the provider request.
	calls := mock.Calls()
	if len(calls) == 0 {
		t.Fatal("no provider calls captured")
	}
	var sawImage bool
	for _, m := range calls[0].Messages {
		for _, p := range m.Parts {
			if p.Type == "image" && p.MIMEType == "image/png" {
				sawImage = true
			}
		}
	}
	if !sawImage {
		t.Fatal("image part did not reach the provider request")
	}
}
