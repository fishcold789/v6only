package v6core

import (
	"bufio"
	"context"
	"github.com/miekg/dns"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestChatGPTDomainBoundary(t *testing.T) {
	for _, h := range []string{"chatgpt.com", "chat.openai.com", "API.OpenAI.com.", "cdn.oaistatic.com"} {
		if !isChatGPT(h) {
			t.Fatal(h)
		}
	}
	for _, h := range []string{"evilopenai.com", "openai.com.evil.test", "www.bilibili.com"} {
		if isChatGPT(h) {
			t.Fatal(h)
		}
	}
}

func TestChatGPTDirectIPv4Exception(t *testing.T) {
	r := New(Config{ChatGPTIPv4: true}, nil)
	defer r.Close()
	r.LookupOverride = func(context.Context, string) (Result, error) {
		return Result{V4: []net.IP{net.ParseIP("192.0.2.3")}, V6: []net.IP{net.ParseIP("2001:db8::3")}}, nil
	}
	r.DialOverride = func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp4" || address != "192.0.2.3:443" {
			t.Errorf("unexpected transport: %s %s", network, address)
		}
		c, peer := net.Pipe()
		peer.Close()
		return c, nil
	}
	c, e := r.Dial(context.Background(), "api.openai.com", "443")
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	if r.Flows("")[0].Reason != "chatgpt_ipv4" {
		t.Fatal(r.Flows(""))
	}
	a, e := r.query(context.Background(), "api.openai.com", dns.TypeAAAA)
	if e != nil || len(a.Answer) != 0 {
		t.Fatal(a, e)
	}
	if _, _, e = r.OpenUDP(context.Background(), "chatgpt.com", "443", []byte("quic")); e == nil {
		t.Fatal("ChatGPT QUIC must not dial IPv6")
	}
}
func TestChatGPTProxyLoopbackFamiliesAndBufferedTunnel(t *testing.T) {
	for _, tc := range []struct{ network, address string }{{"tcp4", "127.0.0.1:0"}, {"tcp6", "[::1]:0"}} {
		t.Run(tc.network, func(t *testing.T) {
			l, e := net.Listen(tc.network, tc.address)
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			target := make(chan string, 1)
			go func() {
				c, e := l.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				q, e := http.ReadRequest(bufio.NewReader(c))
				if e != nil {
					return
				}
				target <- q.Host
				io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\nhello")
				io.Copy(io.Discard, c)
			}()
			r := New(Config{ChatGPTProxy: l.Addr().String()}, nil)
			defer r.Close()
			r.LookupOverride = func(context.Context, string) (Result, error) {
				t.Error("CONNECT must delegate target resolution to the proxy")
				return Result{}, nil
			}
			c, e := r.Dial(context.Background(), "chat.openai.com", "443")
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			b := make([]byte, 5)
			if _, e = io.ReadFull(c, b); e != nil || string(b) != "hello" {
				t.Fatal(string(b), e)
			}
			if <-target != "chat.openai.com:443" {
				t.Fatal("bad CONNECT target")
			}
			f := r.Flows("")[0]
			if f.Network != tc.network+"_proxy" || f.Reason != "chatgpt_proxy" || f.DownloadBytes != 5 {
				t.Fatal(f)
			}
			if _, _, e = r.OpenUDP(context.Background(), "chatgpt.com", "443", []byte("quic")); e == nil {
				t.Fatal("ChatGPT UDP bypass allowed")
			}
			if !strings.Contains(r.ProxyPAC(), "PROXY "+l.Addr().String()) || !strings.Contains(r.ProxyPAC(), "return 'DIRECT'") {
				t.Fatal(r.ProxyPAC())
			}

		})
	}
}

func TestChatGPTProxyPreservesAAAA(t *testing.T) {
	l, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	server := &dns.Server{PacketConn: l, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		a := new(dns.Msg)
		a.SetReply(q)
		if q.Question[0].Qtype == dns.TypeAAAA {
			a.Answer = []dns.RR{&dns.AAAA{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 30}, AAAA: net.ParseIP("2001:db8::8")}}
		}
		w.WriteMsg(a)
	})}
	go server.ActivateAndServe()
	defer server.Shutdown()
	for _, endpoint := range []string{"127.0.0.1:7890", "[::1]:7890"} {
		// An explicit proxy takes precedence over the legacy direct-v4 flag.
		r := New(Config{DNS: []string{l.LocalAddr().String()}, ChatGPTProxy: endpoint, ChatGPTIPv4: true}, nil)
		q := new(dns.Msg)
		q.SetQuestion("chatgpt.com.", dns.TypeAAAA)
		wire, _ := q.Pack()
		answer := new(dns.Msg)
		e := answer.Unpack(r.DNS(context.Background(), wire))
		r.Close()
		if e != nil || answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			t.Fatalf("%s: %v %v", endpoint, answer, e)
		}
		a, ok := answer.Answer[0].(*dns.AAAA)
		if !ok || !a.AAAA.Equal(net.ParseIP("2001:db8::8")) {
			t.Fatal(answer)
		}
	}
}

func TestChatGPTIPv6ProxyFailureDoesNotFallBack(t *testing.T) {
	l, e := net.Listen("tcp6", "[::1]:0")
	if e != nil {
		t.Fatal(e)
	}
	address := l.Addr().String()
	l.Close()
	r := New(Config{ChatGPTProxy: address}, nil)
	defer r.Close()
	r.LookupOverride = func(context.Context, string) (Result, error) {
		t.Error("proxy failure must not resolve a direct route")
		return Result{}, nil
	}
	r.DialOverride = func(context.Context, string, string) (net.Conn, error) {
		t.Error("proxy failure must not dial a direct route")
		return nil, nil
	}
	if c, e := r.Dial(context.Background(), "chatgpt.com", "443"); e == nil {
		c.Close()
		t.Fatal("closed IPv6 proxy unexpectedly succeeded")
	}
	if len(r.Flows("")) != 0 {
		t.Fatal("failed connection recorded as success")
	}
}

func TestChatGPTProxyRejectsNonLoopbackEndpoints(t *testing.T) {
	for _, endpoint := range []string{"[2001:db8::1]:7890", "192.0.2.1:7890", "localhost:7890", "[::]:7890"} {
		r := New(Config{ChatGPTProxy: endpoint}, nil)
		c, e := r.Dial(context.Background(), "chatgpt.com", "443")
		r.Close()
		if e == nil {
			c.Close()
			t.Fatalf("accepted non-loopback endpoint %s", endpoint)
		}
	}
}
