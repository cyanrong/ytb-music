//go:build !darwin

package jsc

import (
	"testing"
	"time"
)

// Non-terminating scripts are interrupted once the execution budget expires.
func TestEvalTimeout(t *testing.T) {
	prev := evalTimeout
	evalTimeout = 40 * time.Millisecond
	defer func() { evalTimeout = prev }()

	if _, err := Eval(`for (;;) {}`); err == nil {
		t.Fatal("infinite loop was not interrupted")
	}
}
