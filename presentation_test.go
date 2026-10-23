package main

import (
	"bytes"
	"testing"
)

func TestRedirectedColorDisabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if colorEnabled(&bytes.Buffer{}) {
		t.Fatal("redirected output must be plain")
	}
}
