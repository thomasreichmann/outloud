package mdns

import (
	"net"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// response builds an answer shaped like mDNSResponder's: the PTR in the
// answers and the SRV, TXT and A records as additionals.
func response(t *testing.T, id uint16, instance string) []byte {
	t.Helper()
	mustName := func(s string) dnsmessage.Name {
		n, err := dnsmessage.NewName(s)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	svc := mustName("_outloud._tcp.local.")
	inst := mustName(instance + "._outloud._tcp.local.")
	host := mustName("Alexs-MacBook-Pro.local.")
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, Authoritative: true})
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	hdr := func(n dnsmessage.Name) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: 10}
	}
	check(b.StartAnswers())
	check(b.PTRResource(hdr(svc), dnsmessage.PTRResource{PTR: inst}))
	check(b.StartAdditionals())
	check(b.SRVResource(hdr(inst), dnsmessage.SRVResource{Target: host, Port: 47811}))
	check(b.TXTResource(hdr(inst), dnsmessage.TXTResource{TXT: []string{"v=1"}}))
	check(b.AResource(hdr(host), dnsmessage.AResource{A: [4]byte{192, 168, 1, 23}}))
	msg, err := b.Finish()
	check(err)
	return msg
}

func TestParsesResponder(t *testing.T) {
	const name = "Alex’s MacBook Pro"
	r := newRecords()
	from := net.IPv4(192, 168, 1, 23)
	r.parse(response(t, 7, name), 7, from)
	r.parse(response(t, 8, "Stray"), 7, from) // answer to someone else's query

	svcs := r.services("_outloud._tcp.local.")
	if len(svcs) != 1 {
		t.Fatalf("got %d services, want 1: %+v", len(svcs), svcs)
	}
	s := svcs[0]
	if s.Instance != name || s.Port != 47811 || s.Host != "Alexs-MacBook-Pro.local." {
		t.Errorf("unexpected service %+v", s)
	}
	if len(s.IPs) != 1 || !s.IPs[0].Equal(from) || len(s.TXT) != 1 || s.TXT[0] != "v=1" {
		t.Errorf("records not attached: %+v", s)
	}
	if got := s.Addr(); got != "192.168.1.23:47811" {
		t.Errorf("Addr() = %q", got)
	}
}

// The Windows mDNS service reports names without the trailing dot, in mixed
// case, maybe escaped, and without a source address.
func TestSystemStyleAnswers(t *testing.T) {
	r := newRecords()
	r.addPTR("_outloud._tcp.local", unescape(`Kitchen\032Mac._outloud._tcp.local`), nil)
	r.addSRV("kitchen mac._outloud._tcp.local", "kitchen.local", 47811)
	r.addA("kitchen.local", net.IPv4(169, 254, 3, 7))
	r.addA("Kitchen.local", net.IPv4(10, 0, 0, 5))

	svcs := r.services("_outloud._tcp.local.")
	if len(svcs) != 1 || svcs[0].Instance != "Kitchen Mac" || len(svcs[0].IPs) != 2 {
		t.Fatalf("got %+v, want one Kitchen Mac with two addresses", svcs)
	}
	// The link-local address loses to the routable one.
	if got := svcs[0].Addr(); got != "10.0.0.5:47811" {
		t.Errorf("Addr() = %q", got)
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`Plain`:          "Plain",
		`Kitchen\032Mac`: "Kitchen Mac",
		`My\.Mac`:        "My.Mac",
		`trailing\`:      `trailing\`,
	} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}
