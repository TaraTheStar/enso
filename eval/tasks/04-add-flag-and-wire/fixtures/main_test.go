package main

import (
	"bytes"
	"os"
	"testing"
)

func TestRun_DefaultGreeting(t *testing.T) {
	r, w, _ := os.Pipe()
	go func() {
		Run(Config{Greeting: "hi"}, w)
		w.Close()
	}()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	if buf.String() != "hi\n" {
		t.Errorf("got %q, want %q", buf.String(), "hi\n")
	}
}
