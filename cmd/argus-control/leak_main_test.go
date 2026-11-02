package main

import (
	"testing"

	"go.uber.org/goleak"
)

// Every test in the package leaves no goroutine behind it. A goroutine that
// outlives what started it holds everything it references, so a leak here is
// memory that grows for as long as the agent or control plane runs.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
