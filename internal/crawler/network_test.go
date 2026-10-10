package crawler

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestSafeDialPinsDNSAndRejectsRebinding(t *testing.T) {
	lookups, dials := 0, 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		ip := "93.184.216.34"
		if lookups > 1 {
			ip = "127.0.0.1"
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	dial := safeDialContext(false, lookup, func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "93.184.216.34:443" {
			t.Fatalf("dial used unvalidated DNS name: %s", address)
		}
		client, server := net.Pipe()
		server.Close()
		return client, nil
	})
	conn, err := dial(context.Background(), "tcp", "rebind.example:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if lookups != 1 || dials != 1 {
		t.Fatalf("lookup/dial count: %d/%d", lookups, dials)
	}
	if _, err = dial(context.Background(), "tcp", "rebind.example:443"); err == nil || dials != 1 {
		t.Fatal("rebound private address reached the dialer")
	}
}

func TestRobotsRedirectCannotDialPrivateDestination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://internal.example/robots.txt", http.StatusFound)
	}))
	defer server.Close()
	dials := 0
	transport := NewHTTPTransport(TransportConfig{Workers: 1}, 0)
	defer transport.CloseIdleConnections()
	transport.DialContext = safeDialContext(false, func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host == "public.example" {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "93.184.216.34:80" {
			return nil, fmt.Errorf("unexpected dial %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	response, err := NewRobotsHTTPClient(transport, 5).Get("http://public.example/robots.txt")
	if response != nil {
		response.Body.Close()
	}
	if err == nil || dials != 1 {
		t.Fatalf("private redirect was not blocked: dials=%d error=%v", dials, err)
	}
}

func TestBlockedUnusualIPv6Addresses(t *testing.T) {
	for _, address := range []string{
		"::ffff:127.0.0.1", "::ffff:192.168.1.1", "fe80::1%eth0",
		"fc00::1234", "64:ff9b::a9fe:a9fe", "64:ff9b:1::808:808",
		"2002:7f00:1::", "2001::1", "2001:db8::1",
	} {
		t.Run(address, func(t *testing.T) {
			if !IsBlockedTargetIP(netip.MustParseAddr(address)) {
				t.Fatalf("special IPv6 address accepted: %s", address)
			}
		})
	}
	if err := ValidateResolvedTargetIPs("mixed.example", []netip.Addr{
		netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("::ffff:10.0.0.1"),
	}, false); err == nil {
		t.Fatal("mixed public/private answer accepted")
	}
}
