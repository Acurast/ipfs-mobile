package utils

import "testing"

// The Kotlin wrapper turns anything that is not a size-limit error into an
// IOException and surfaces this message, so the wording is part of the contract.
func TestTimeout(t *testing.T) {
	err := Timeout()

	if err == nil {
		t.Fatal("Timeout() returned nil")
	}
	if err.Error() != "timeout" {
		t.Errorf("Timeout() = %q, want %q", err.Error(), "timeout")
	}
}
