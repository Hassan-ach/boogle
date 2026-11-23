package utils

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestIsPublicIPRejectsEveryNonRoutableRange(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"0.0.0.0",
		"10.1.2.3",
		"172.16.0.9",
		"192.168.1.1",
		"169.254.169.254", // cloud metadata
		"100.64.0.1",      // CGNAT
		"198.18.0.1",      // benchmarking
		"192.0.0.8",       // IETF protocol assignments
		"198.51.100.4",    // TEST-NET-2
		"203.0.113.4",     // TEST-NET-3
		"224.0.0.1",       // multicast
		"240.0.0.1",       // reserved
		"::1",
		"fc00::1",
		"fe80::1",
		"ff02::1",
		"2001:db8::1",
		"64:ff9b::7f00:1",    // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe", // NAT64 of 169.254.169.254
		"2002:7f00:1::",      // 6to4 of 127.0.0.1
		"2001::1",            // Teredo
		"fec0::1",            // deprecated site-local
	}
	for _, raw := range blocked {
		if IsPublicIP(net.ParseIP(raw)) {
			t.Errorf("IsPublicIP(%s) = true, want false", raw)
		}
	}
}

func TestIsPublicIPAcceptsOrdinaryPublicAddresses(t *testing.T) {
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if !IsPublicIP(net.ParseIP(raw)) {
			t.Errorf("IsPublicIP(%s) = false, want true", raw)
		}
	}
}

func TestIsPublicIPRejectsNil(t *testing.T) {
	if IsPublicIP(nil) {
		t.Error("IsPublicIP(nil) = true")
	}
}

func TestTheGuardedDialerRefusesLoopbackBeforeConnecting(t *testing.T) {
	dial := PublicOnlyDialContext(&net.Dialer{Timeout: time.Second})

	_, err := dial(context.Background(), "tcp", "127.0.0.1:9")

	if err == nil {
		t.Fatal("the guarded dialer connected to a loopback address")
	}
}

func TestTheGuardedDialerRefusesTheMetadataAddress(t *testing.T) {
	dial := PublicOnlyDialContext(&net.Dialer{Timeout: time.Second})

	_, err := dial(context.Background(), "tcp", "169.254.169.254:80")

	if err == nil {
		t.Fatal("the guarded dialer connected to the cloud metadata address")
	}
}
