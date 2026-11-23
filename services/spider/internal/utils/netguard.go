package utils

import (
	"context"
	"fmt"
	"net"
	"time"
)

// PublicOnlyDialContext returns a dial function that refuses non-public addresses,
// including internal services and cloud metadata endpoints. It resolves and dials
// the same IP to avoid a DNS rebinding gap.
func PublicOnlyDialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid dial address %q: %w", addr, err)
		}

		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for %q", host)
		}

		var lastErr error
		for _, candidate := range ips {
			if !IsPublicIP(candidate.IP) {
				lastErr = fmt.Errorf("refusing to connect to non-public address %s for %s", candidate.IP, host)
				continue
			}
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no public address for %q", host)
		}
		return nil, lastErr
	}
}

// IsPublicIP reports whether ip is a globally routable unicast address.
//
// `net.IP.IsPrivate` covers RFC 1918 and IPv6 ULA but not loopback, link-local,
// CGNAT, benchmarking, reserved, or 0.0.0.0/8, so each is excluded explicitly.
func IsPublicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}

	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // "this network"
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24 IETF
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15 benchmarking
			return false
		case v4[0] == 198 && v4[1] == 51 && v4[2] == 100: // 198.51.100.0/24 TEST-NET-2
			return false
		case v4[0] == 203 && v4[1] == 0 && v4[2] == 113: // 203.0.113.0/24 TEST-NET-3
			return false
		case v4[0] >= 240: // 240.0.0.0/4 reserved, includes 255.255.255.255
			return false
		}
		return true
	}

	// IPv6. IsPrivate covers fc00::/7; reject documentation and discard space
	// explicitly. 2001:db8::/32 is documentation-only and never a real origin.
	if len(ip) == net.IPv6len {
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
			return false
		}
		if ip[0] == 0x01 && ip[1] == 0x00 && ip[2] == 0 && ip[3] == 0 &&
			ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 { // 100::/64 discard
			return false
		}
		// Translation and transition ranges can carry an IPv4 destination, so
		// accepting them would let 64:ff9b::7f00:1 reach 127.0.0.1 on a
		// NAT64/6to4 network.
		if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b { // 64:ff9b::/96 NAT64
			return false
		}
		if ip[0] == 0x20 && ip[1] == 0x02 { // 2002::/16 6to4
			return false
		}
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x00 { // 2001::/32 Teredo
			return false
		}
		if ip[0] == 0xfe && ip[1] >= 0xc0 && ip[1] <= 0xff { // fec0::/10 deprecated site-local
			return false
		}
	}
	return true
}
