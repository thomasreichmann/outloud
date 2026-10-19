// Package mdns finds and advertises DNS-SD services on the local network.
//
// On Windows, Browse asks the system's DNS Client service (DnsServiceBrowse),
// which owns port 5353 and has its own firewall rule. Elsewhere it falls back
// to "legacy unicast" queries (RFC 6762 §6.7): the query goes to the mDNS
// group from an ordinary ephemeral port, to be answered by unicast. macOS's
// mDNSResponder does not answer those when they are sent to the group, so
// the fallback only serves tests and other responders.
package mdns

import (
	"context"
	"math/rand/v2"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Service is one discovered service instance.
type Service struct {
	Instance string // human-readable instance name, e.g. "Alex's MacBook Pro"
	Host     string // target host name, e.g. "Alexs-MacBook-Pro.local."
	Port     int
	IPs      []net.IP // IPv4 addresses the responder gave for Host
	Src      net.IP   // address the answer came from
	TXT      []string
}

// Addr returns a dialable host:port. The answer's source address is
// preferred: it is known to be reachable from here, while a multi-homed
// host also lists addresses on other networks, such as a Mac's link-local
// Thunderbolt bridge. Without one, an address on a local subnet wins.
func (s Service) Addr() string {
	ip, best := s.Src, 3
	if ip == nil {
		for _, a := range s.IPs {
			rank := 2
			if !a.IsLinkLocalUnicast() {
				rank = 1
				if onLocalSubnet(a) {
					rank = 0
				}
			}
			if rank < best {
				ip, best = a, rank
			}
		}
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(s.Port))
}

func onLocalSubnet(ip net.IP) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.Contains(ip) {
			return true
		}
	}
	return false
}

// Browse looks for instances of service (e.g. "_outloud._tcp") for the
// given duration and returns what answered, sorted by instance name.
func Browse(ctx context.Context, service string, wait time.Duration) ([]Service, error) {
	fqdn := strings.TrimSuffix(service, ".") + ".local."
	recs, ok := systemBrowse(ctx, fqdn, wait)
	if !ok {
		var err error
		if recs, err = query(ctx, fqdn, wait); err != nil {
			return nil, err
		}
	}
	return recs.services(fqdn), nil
}

// query sends legacy unicast queries and collects the answers.
func query(ctx context.Context, fqdn string, wait time.Duration) (*records, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetMulticastTTL(255)

	id := uint16(rand.Uint32())
	query, err := buildQuery(id, fqdn)
	if err != nil {
		return nil, err
	}
	ifaces := multicastInterfaces()
	send := func() {
		if len(ifaces) == 0 {
			_, _ = pc.WriteTo(query, nil, group)
			return
		}
		for i := range ifaces {
			if pc.SetMulticastInterface(&ifaces[i]) == nil {
				_, _ = pc.WriteTo(query, nil, group)
			}
		}
	}

	// Close the socket if ctx ends early, which unblocks the read.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	recs := newRecords()
	deadline := time.Now().Add(wait)
	// Queries go out three times: mDNS is unreliable by design.
	resend := []time.Duration{0, 250 * time.Millisecond, 750 * time.Millisecond}
	start := time.Now()
	buf := make([]byte, 9000)
	for {
		for len(resend) > 0 && time.Since(start) >= resend[0] {
			send()
			resend = resend[1:]
		}
		until := deadline
		if len(resend) > 0 && start.Add(resend[0]).Before(until) {
			until = start.Add(resend[0])
		}
		if !time.Now().Before(deadline) {
			break
		}
		_ = conn.SetReadDeadline(until)
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		recs.parse(buf[:n], id, src.IP)
	}
	return recs, nil
}

func buildQuery(id uint16, fqdn string) ([]byte, error) {
	name, err := dnsmessage.NewName(fqdn)
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	return b.Finish()
}

func multicastInterfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

type srv struct {
	target string
	port   int
}

// records accumulates resource records from all answers, keyed by
// lower-cased owner name.
type records struct {
	ptr map[string][]string // service name -> instance names
	srv map[string]srv
	txt map[string][]string
	a   map[string][]net.IP
	src map[string]net.IP // instance name -> responder address
}

func newRecords() *records {
	return &records{
		ptr: map[string][]string{},
		srv: map[string]srv{},
		txt: map[string][]string{},
		a:   map[string][]net.IP{},
		src: map[string]net.IP{},
	}
}

// parse adds the records of one response to query id, received from from.
func (r *records) parse(msg []byte, id uint16, from net.IP) {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || !h.Response || h.ID != id {
		return
	}
	if err := p.SkipAllQuestions(); err != nil {
		return
	}
	var all []dnsmessage.Resource
	for _, section := range []func() ([]dnsmessage.Resource, error){p.AllAnswers, p.AllAuthorities, p.AllAdditionals} {
		rs, err := section()
		all = append(all, rs...)
		if err != nil {
			break // keep what parsed
		}
	}
	for _, rr := range all {
		owner := rr.Header.Name.String()
		switch b := rr.Body.(type) {
		case *dnsmessage.PTRResource:
			r.addPTR(owner, b.PTR.String(), from)
		case *dnsmessage.SRVResource:
			r.addSRV(owner, b.Target.String(), int(b.Port))
		case *dnsmessage.TXTResource:
			r.addTXT(owner, b.TXT)
		case *dnsmessage.AResource:
			r.addA(owner, net.IP(b.A[:]))
		}
	}
}

// canon gives a name its trailing dot; key also lower-cases it for lookups.
func canon(name string) string {
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}

func key(name string) string { return strings.ToLower(canon(name)) }

// unescape undoes DNS presentation-format escapes (\032, \.), which the
// Windows mDNS service may use in names.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] != '\\' || i+1 == len(s):
			b = append(b, s[i])
		case i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]):
			b = append(b, (s[i+1]-'0')*100+(s[i+2]-'0')*10+(s[i+3]-'0'))
			i += 3
		default:
			b = append(b, s[i+1])
			i++
		}
	}
	return string(b)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// addPTR records an instance of a service; from is the responder's
// address, if known.
func (r *records) addPTR(service, instance string, from net.IP) {
	instance = canon(instance)
	if !slices.ContainsFunc(r.ptr[key(service)], func(s string) bool { return strings.EqualFold(s, instance) }) {
		r.ptr[key(service)] = append(r.ptr[key(service)], instance)
	}
	if from != nil {
		r.src[key(instance)] = from
	}
}

func (r *records) addSRV(instance, target string, port int) {
	r.srv[key(instance)] = srv{target: canon(target), port: port}
}

func (r *records) addTXT(instance string, txt []string) { r.txt[key(instance)] = txt }

func (r *records) addA(host string, ip net.IP) {
	if !slices.ContainsFunc(r.a[key(host)], ip.Equal) {
		r.a[key(host)] = append(r.a[key(host)], ip)
	}
}

func (r *records) services(fqdn string) []Service {
	var out []Service
	suffix := "." + fqdn
	for _, inst := range r.ptr[key(fqdn)] {
		k := key(inst)
		s, ok := r.srv[k]
		if !ok {
			continue
		}
		name := inst
		if len(name) > len(suffix) && strings.EqualFold(name[len(name)-len(suffix):], suffix) {
			name = name[:len(name)-len(suffix)]
		}
		out = append(out, Service{
			Instance: name,
			Host:     s.target,
			Port:     s.port,
			IPs:      r.a[key(s.target)],
			Src:      r.src[k],
			TXT:      r.txt[k],
		})
	}
	slices.SortFunc(out, func(a, b Service) int { return strings.Compare(a.Instance, b.Instance) })
	return out
}
