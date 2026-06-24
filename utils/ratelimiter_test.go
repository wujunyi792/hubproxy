package utils

import "testing"

func TestExtractIPFromAddress(t *testing.T) {
	if got := extractIPFromAddress("127.0.0.1:5000"); got != "127.0.0.1" {
		t.Fatalf("extract IPv4 = %q", got)
	}
	if got := extractIPFromAddress("[2001:db8::1]:5000"); got != "2001:db8::1" {
		t.Fatalf("extract IPv6 = %q", got)
	}
}

func TestNormalizeIPv6ForRateLimit(t *testing.T) {
	if got := normalizeIPForRateLimit("192.168.1.2"); got != "192.168.1.2" {
		t.Fatalf("IPv4 normalized = %q", got)
	}
	if got := normalizeIPForRateLimit("2001:db8::1"); got != "2001:db8::/64" {
		t.Fatalf("IPv6 normalized = %q", got)
	}
}
