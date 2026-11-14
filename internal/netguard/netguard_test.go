package netguard

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestIsPublicAddrBlocksPrivateAndMetadataRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"10.0.0.5",
		"172.16.10.1",
		"192.168.1.20",
		"169.254.169.254", // Cloud metadata service
		"100.64.1.1",      // CGNAT
		"0.0.0.0",
		"::1",
		"fe80::1",
		"fc00::1234",
	}
	for _, raw := range blocked {
		ip := netip.MustParseAddr(raw)
		if IsPublicAddr(ip) {
			t.Errorf("expected %s to be blocked by SSRF guard", raw)
		}
	}

	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, raw := range public {
		ip := netip.MustParseAddr(raw)
		if !IsPublicAddr(ip) {
			t.Errorf("expected %s to be permitted as public IP", raw)
		}
	}
}

func TestSafeClientRejectsDisallowedHostAndLoopback(t *testing.T) {
	client := NewSafeClient(Options{})
	_, _, _, err := client.GetBytes(context.Background(), "http://169.254.169.254/latest/meta-data/", nil)
	if !errors.Is(err, ErrBlockedHost) {
		t.Fatalf("expected ErrBlockedHost for metadata IP host, got %v", err)
	}

	// Even if a host is added to allowlist, resolving to loopback must be blocked when AllowLoopbackTest=false
	clientLoop := NewSafeClient(Options{
		AllowedHosts: map[string]bool{"localhost": true},
	})
	_, _, _, err = clientLoop.GetBytes(context.Background(), "http://localhost:8080/secret", nil)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected ErrBlockedAddress for loopback dial, got %v", err)
	}
}
