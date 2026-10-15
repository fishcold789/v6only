package v6core

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestExplicitIPv4Fallback(t *testing.T) {
	for _, allow := range []bool{false, true} {
		r := New(Config{AllowIPv4Fallback: allow, FamilyTimeoutMS: 100}, nil)
		r.LookupOverride = func(context.Context, string) (Result, error) { return dual(), nil }
		var mu sync.Mutex
		var calls []string
		r.DialOverride = func(ctx context.Context, n, a string) (net.Conn, error) {
			mu.Lock()
			calls = append(calls, n)
			mu.Unlock()
			if n == "tcp6" {
				return nil, errors.New("unreachable IPv6")
			}
			c, p := net.Pipe()
			p.Close()
			return c, nil
		}
		c, err := r.Dial(context.Background(), "dual.example", "443")
		if allow {
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			if flows := r.Flows(""); len(flows) != 1 || flows[0].Reason != "ipv6_failed_fallback" || flows[0].Network != "tcp4" {
				t.Fatal(flows)
			}
		} else if err == nil {
			t.Fatal("strict default allowed fallback")
		}
		mu.Lock()
		got := strings.Join(calls, ",")
		mu.Unlock()
		want := "tcp6"
		if allow {
			want += ",tcp4"
		}
		if got != want {
			t.Fatalf("%v: %s", allow, got)
		}
		r.Close()
	}
}

func TestFallbackDoesNotHideDNSFailureOrCancellation(t *testing.T) {
	for _, dnsFailure := range []bool{false, true} {
		r := New(Config{AllowIPv4Fallback: true}, nil)
		r.LookupOverride = func(context.Context, string) (Result, error) {
			if dnsFailure {
				return Result{}, errors.New("AAAA failed")
			}
			return dual(), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.DialOverride = func(context.Context, string, string) (net.Conn, error) { cancel(); return nil, context.Canceled }
		if c, err := r.Dial(ctx, "dual.example", "443"); err == nil {
			c.Close()
			t.Fatal("failure was hidden")
		}
		r.Close()
		cancel()
	}
}

func TestExplicitUDPFallback(t *testing.T) {
	for _, allow := range []bool{false, true} {
		r := New(Config{AllowIPv4Fallback: allow, FamilyTimeoutMS: 100}, nil)
		r.LookupOverride = func(context.Context, string) (Result, error) { return dual(), nil }
		calls := []string{}
		r.DialOverride = func(ctx context.Context, n, a string) (net.Conn, error) {
			calls = append(calls, n)
			if n == "udp6" {
				return nil, errors.New("network unreachable")
			}
			c, p := net.Pipe()
			go func() { defer p.Close(); b := make([]byte, 32); p.Read(b); p.Write([]byte("reply")) }()
			return c, nil
		}
		c, reply, err := r.OpenUDP(context.Background(), "dual.example", "443", []byte("quic"))
		if allow {
			if err != nil || string(reply) != "reply" {
				t.Fatal(err, string(reply))
			}
			c.Close()
			if r.Flows("")[0].Reason != "ipv6_failed_fallback" {
				t.Fatal("missing fallback reason")
			}
		} else if err == nil {
			t.Fatal("strict UDP allowed fallback")
		}
		want := "udp6"
		if allow {
			want += ",udp4"
		}
		if strings.Join(calls, ",") != want {
			t.Fatal(calls)
		}
		r.Close()
	}
}
