package web

import (
	"os/exec"
	"testing"
)

// TestClientQueue runs the Node tests for the page's action queue. Node is a
// development tool only; the app has no Node runtime. Without it the test is
// skipped.
func TestClientQueue(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; skipping web/test/*.test.js")
	}
	out, err := exec.Command(node, "--test", "test/queue.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node tests failed:\n%s", out)
	}
}
