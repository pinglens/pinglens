package target

import (
	"errors"
	"testing"
)

func TestNormalizeAcceptsPublicTargets(t *testing.T) {
	tests := []string{"Example.COM.", "8.8.8.8", "2606:4700:4700::1111"}
	for _, test := range tests {
		if _, err := Normalize(test); err != nil {
			t.Fatalf("Normalize(%q) failed: %v", test, err)
		}
	}
}

func TestNormalizeRejectsURLsAndPrivateAddresses(t *testing.T) {
	tests := []string{"https://example.com", "example.com/path", "127.0.0.1", "10.0.0.1", "192.168.1.1", "fc00::1", "localhost"}
	for _, test := range tests {
		if _, err := Normalize(test); err == nil {
			t.Fatalf("Normalize(%q) succeeded unexpectedly", test)
		}
	}
}

func TestPrivateAddressError(t *testing.T) {
	_, err := Normalize("10.0.0.1")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected ErrBlockedAddress, got %v", err)
	}
}
