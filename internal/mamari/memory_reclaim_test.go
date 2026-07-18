package mamari

import "testing"

// TestReleaseUnusedMemoryDoesNotPanic guards the one remaining call site
// (mcpserver.ServeWithOptions / cmd/mamari's runUI, both calling this once
// at startup before serving any request) against a panic on a freshly
// constructed Index — see ReleaseUnusedMemory's doc comment for why this is
// a single, unconditional, startup-only call rather than something wired
// into search_code/semantic's lazy build paths (an earlier version did
// that; removed after measuring its incremental benefit was marginal on
// Linux while its occasional latency cost during an active session was
// real).
func TestReleaseUnusedMemoryDoesNotPanic(t *testing.T) {
	idx := &Index{}
	idx.ReleaseUnusedMemory()
}
