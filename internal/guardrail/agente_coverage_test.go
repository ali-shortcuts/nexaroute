package guardrail

import "testing"

// Agent E coverage: validCardNumber's non-digit guard (scan.go:46-47).
// The credit-card regex only yields digits, spaces and dashes, so Scan
// itself can never reach this branch; the validator is tested directly
// to pin its defensive fail-closed behavior on a non-digit candidate.
func TestAgentEValidCardNumberRejectsNonDigit(t *testing.T) {
	// 14 characters after stripping, so the length gate passes and the
	// loop must hit the non-digit 'x'.
	if validCardNumber("4111111111111x") {
		t.Fatal("non-digit candidate accepted as a card number")
	}
	// Sanity: a Luhn-valid number is still accepted, and a Luhn-invalid
	// all-digit candidate is rejected by the checksum, not the guard.
	if !validCardNumber("4111111111111111") {
		t.Fatal("Luhn-valid test number rejected")
	}
	if validCardNumber("4111111111111112") {
		t.Fatal("Luhn-invalid number accepted")
	}
}
