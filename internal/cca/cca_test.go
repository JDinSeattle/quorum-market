package cca

import "testing"

func TestValidCardNumber(t *testing.T) {
	valid := []string{
		"1234-5678-9012-3456",
		"1234567890123456",
		"1234 5678 9012 3456",
	}
	for _, in := range valid {
		if !ValidCardNumber(in) {
			t.Errorf("ValidCardNumber(%q) = false, want true", in)
		}
	}

	invalid := []string{
		"",
		"1234",
		"12345678901234567",
		"1234-5678-9012-345x",
		"not-a-card",
	}
	for _, in := range invalid {
		if ValidCardNumber(in) {
			t.Errorf("ValidCardNumber(%q) = true, want false", in)
		}
	}
}
