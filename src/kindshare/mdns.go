package main

// A purpose-built mDNS responder for our one service.
//
// We advertised with grandcat/zeroconf up to now. It works with Android and
// with the Windows Quick Share app; it does not work reliably with macOS, which
// runs the strictest Bonjour implementation in the room. Three of its
// properties are responsible, in descending order of how much they matter:
//
//   - It never answers address queries. Its question dispatch matches exactly
//     three names - the service-type enumeration, the service, and the service
//     instance - and ignores the question type. A querier that resolves the
//     instance, reads the SRV target and then asks for the A record of that
//     host name gets silence. Our address only ever rides along as a bonus
//     record attached to some other answer, with a hardcoded 120-second TTL,
//     so once that ages out of the Mac's cache the service is still listed but
//     can no longer be reached. NearDrop dials the Bonjour endpoint rather than
//     an address, so this is exactly the lookup it depends on.
//
//   - It never re-announces. One burst at registration and then nothing but
//     replies to queries. On a link that drops multicast - which is every
//     Kindle, especially one that keeps dozing off - a lost reply means the
//     device stays invisible until something forces a re-registration.
//
//   - It sends with the default multicast TTL of 1 instead of the 255 that
//     RFC 6762 section 11 requires. Queriers are explicitly allowed to discard
//     anything else, and it costs nothing to comply.
//
// So this file answers queries for our host name, re-announces on a timer, and
// sets the TTL. It advertises exactly one service, which is why it is short.

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

const (
	mdnsPort = 5353

	// Top bit of an rrclass in a response: "throw away anything else you have
	// cached under this name". Set it on the records only we can own, never on
	// the shared PTR that every device of this type appends itself to.
	cacheFlush = 1 << 15

	// Top bit of a qclass in a question: "answer me directly, not to the group".
	unicastBit = 1 << 15

	// Apple's own responder uses 4500s for the records that say what exists and
	// 120s for the ones that say where it is, so a querier holds our entry for
	// as long as it would hold a real Mac's.
	ttlDescribe = 4500
	ttlLocate   = 120

	// How often to re-announce unprompted. Comfortably inside the 120s address
	// TTL, so a Mac's cache never goes stale even if every query we should have
	// answered was lost.
	defaultAnnounceEvery = 45 * time.Second
)

var mdnsGroupV4 = net.IPv4(224, 0, 0, 251)

// advertiser publishes one DNS-SD service and the host name its SRV points at.
type advertiser struct {
	instance string // service instance label, no dots
	service  string // "_FC9F5ED42C8A._tcp"
	domain   string // "local."
	host     string // host label, no dots
	port     int
	txt      []string
	iface    *net.Interface
	every    time.Duration

	mu   sync.Mutex
	ip   net.IP
	conn *ipv4.PacketConn

	stop chan struct{}
	kick chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func (a *advertiser) serviceName() string  { return dns.Fqdn(a.service + "." + a.domain) }
func (a *advertiser) instanceName() string { return dns.Fqdn(a.instance + "." + a.service + "." + a.domain) }
func (a *advertiser) hostName() string     { return dns.Fqdn(a.host + "." + a.domain) }
func (a *advertiser) enumName() string     { return dns.Fqdn("_services._dns-sd._udp." + a.domain) }

func (a *advertiser) addr() net.IP {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ip
}

// ---------------------------------------------------------------- records

func (a *advertiser) ptrEnum(ttl uint32) dns.RR {
	return &dns.PTR{
		Hdr: dns.RR_Header{Name: a.enumName(), Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: a.serviceName(),
	}
}

func (a *advertiser) ptrService(ttl uint32) dns.RR {
	// Shared record: no cache-flush bit, or we would erase every other device
	// advertising the same service type.
	return &dns.PTR{
		Hdr: dns.RR_Header{Name: a.serviceName(), Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: a.instanceName(),
	}
}

func (a *advertiser) srv(ttl uint32) dns.RR {
	return &dns.SRV{
		Hdr:    dns.RR_Header{Name: a.instanceName(), Rrtype: dns.TypeSRV, Class: dns.ClassINET | cacheFlush, Ttl: ttl},
		Port:   uint16(a.port),
		Target: a.hostName(),
	}
}

func (a *advertiser) txtRR(ttl uint32) dns.RR {
	return &dns.TXT{
		Hdr: dns.RR_Header{Name: a.instanceName(), Rrtype: dns.TypeTXT, Class: dns.ClassINET | cacheFlush, Ttl: ttl},
		Txt: a.txt,
	}
}

// aRR is nil when we have no address, which is how "the network is down" is
// expressed: we answer nothing rather than pointing at a dead host.
func (a *advertiser) aRR(ttl uint32) dns.RR {
	ip := a.addr()
	if ip == nil {
		return nil
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	return &dns.A{
		Hdr: dns.RR_Header{Name: a.hostName(), Rrtype: dns.TypeA, Class: dns.ClassINET | cacheFlush, Ttl: ttl},
		A:   v4,
	}
}

// fullSet is everything we own, in the order a querier most wants it.
func (a *advertiser) fullSet(describe, locate uint32) []dns.RR {
	rrs := []dns.RR{a.ptrService(describe), a.srv(locate), a.txtRR(describe), a.ptrEnum(describe)}
	if rr := a.aRR(locate); rr != nil {
		rrs = append(rrs, rr)
	}
	return rrs
}

// ---------------------------------------------------------------- answering

// answersFor decides what a single question is entitled to.
func (a *advertiser) answersFor(q dns.Question) (ans, extra []dns.RR) {
	wants := func(t uint16) bool { return q.Qtype == t || q.Qtype == dns.TypeANY }
	add := func(dst []dns.RR, rr dns.RR) []dns.RR {
		if rr == nil {
			return dst
		}
		return append(dst, rr)
	}

	switch strings.ToLower(dns.Fqdn(q.Name)) {
	case strings.ToLower(a.enumName()):
		if wants(dns.TypePTR) {
			ans = append(ans, a.ptrEnum(ttlDescribe))
		}

	case strings.ToLower(a.serviceName()):
		// The browse. Attach everything needed to act on it so a well-behaved
		// querier never has to ask again.
		if wants(dns.TypePTR) {
			ans = append(ans, a.ptrService(ttlDescribe))
			extra = append(extra, a.srv(ttlLocate), a.txtRR(ttlDescribe))
			extra = add(extra, a.aRR(ttlLocate))
		}

	case strings.ToLower(a.instanceName()):
		if wants(dns.TypeSRV) {
			ans = append(ans, a.srv(ttlLocate))
			extra = add(extra, a.aRR(ttlLocate))
		}
		if wants(dns.TypeTXT) {
			ans = append(ans, a.txtRR(ttlDescribe))
		}

	case strings.ToLower(a.hostName()):
		// The one the old library never answered.
		if wants(dns.TypeA) {
			ans = add(ans, a.aRR(ttlLocate))
		}
	}
	return ans, extra
}

// respond answers one query. Questions asking for a direct reply are answered
// directly and the rest go to the group, so a querier that set the unicast bit
// on its first probe hears back immediately.
func (a *advertiser) respond(q *dns.Msg, src net.Addr) {
	var uniAns, uniExtra, mcAns, mcExtra []dns.RR
	for _, question := range q.Question {
		ans, extra := a.answersFor(question)
		if len(ans) == 0 {
			continue
		}
		if question.Qclass&unicastBit != 0 {
			uniAns, uniExtra = append(uniAns, ans...), append(uniExtra, extra...)
		} else {
			mcAns, mcExtra = append(mcAns, ans...), append(mcExtra, extra...)
		}
	}
	if len(uniAns) > 0 {
		a.send(reply(uniAns, uniExtra), src)
	}
	if len(mcAns) > 0 {
		a.send(reply(mcAns, mcExtra), nil)
	}
}

// reply builds a response message. RFC 6762 section 6: the id is zero and the
// question section is not echoed back.
func reply(ans, extra []dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.Response = true
	m.Authoritative = true
	m.Compress = true
	m.Answer = ans
	m.Extra = extra
	return m
}

// send writes a message, to dst if given and to the group otherwise.
func (a *advertiser) send(m *dns.Msg, dst net.Addr) {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return
	}
	b, err := m.Pack()
	if err != nil {
		log.Printf("mdns: pack: %v", err)
		return
	}
	var cm *ipv4.ControlMessage
	if a.iface != nil {
		cm = &ipv4.ControlMessage{IfIndex: a.iface.Index}
	}
	to := dst
	if to == nil {
		to = &net.UDPAddr{IP: mdnsGroupV4, Port: mdnsPort}
	}
	if _, err := conn.WriteTo(b, cm, to); err != nil {
		log.Printf("mdns: send to %v: %v", to, err)
	}
}

// ---------------------------------------------------------------- lifecycle

func (a *advertiser) start() error {
	if a.every == 0 {
		a.every = defaultAnnounceEvery
	}
	a.stop = make(chan struct{})
	a.kick = make(chan struct{}, 1)

	if err := a.rebind(); err != nil {
		return err
	}
	a.wg.Add(1)
	go a.announceLoop()

	log.Printf("mdns: %s -> %s:%d, announcing every %s",
		a.instanceName(), a.hostName(), a.port, a.every)
	return nil
}

// open builds a socket that can both hear the group and answer from port 5353,
// which is where responses are required to come from. The reuse-address option
// lets us sit alongside anything else on the device holding the port.
func (a *advertiser) open() (*ipv4.PacketConn, error) {
	lc := net.ListenConfig{Control: reuseAddr}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", mdnsPort))
	if err != nil {
		return nil, fmt.Errorf("bind :%d: %w", mdnsPort, err)
	}
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		pc.Close()
		return nil, fmt.Errorf("bind :%d: not a UDP socket", mdnsPort)
	}

	p := ipv4.NewPacketConn(uc)
	if a.iface != nil {
		if err := p.SetMulticastInterface(a.iface); err != nil {
			log.Printf("mdns: multicast interface %s: %v", a.iface.Name, err)
		}
	}
	if err := p.JoinGroup(a.iface, &net.UDPAddr{IP: mdnsGroupV4}); err != nil {
		p.Close()
		return nil, fmt.Errorf("join 224.0.0.251: %w", err)
	}
	// RFC 6762 section 11: 255, or a querier is entitled to drop us.
	if err := p.SetMulticastTTL(255); err != nil {
		log.Printf("mdns: multicast ttl: %v", err)
	}
	if err := p.SetTTL(255); err != nil {
		log.Printf("mdns: ttl: %v", err)
	}
	p.SetMulticastLoopback(true)
	return p, nil
}

// rebind replaces the socket. Membership of a multicast group belongs to the
// socket and the interface together, so anything that takes wlan0 down and
// brings it back - a wifi toggle, the SoftAP, a resume - can leave us holding a
// socket that still reads and writes and no longer hears a thing. Nothing about
// that state is observable from inside, so we rebuild rather than check.
func (a *advertiser) rebind() error {
	p, err := a.open()
	if err != nil {
		return err
	}
	a.mu.Lock()
	old := a.conn
	a.conn = p
	a.mu.Unlock()
	if old != nil {
		old.Close() // its reader sees the close and exits
	}
	a.wg.Add(1)
	go a.serve(p)
	return nil
}

// refresh rebuilds the socket and announces again, for when the address has not
// changed but our position on the network has.
func (a *advertiser) refresh() {
	if err := a.rebind(); err != nil {
		log.Printf("mdns: rebind: %v", err)
		return
	}
	a.kickAnnounce()
}

func (a *advertiser) kickAnnounce() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

func (a *advertiser) serve(conn *ipv4.PacketConn) {
	defer a.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, _, src, err := conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-a.stop:
				return
			default:
			}
			// Also the normal exit for a socket rebind replaced underneath us.
			a.mu.Lock()
			current := a.conn
			a.mu.Unlock()
			if current != conn {
				return
			}
			log.Printf("mdns: read: %v", err)
			return
		}
		m := new(dns.Msg)
		if err := m.Unpack(buf[:n]); err != nil {
			continue
		}
		if m.Response || len(m.Question) == 0 {
			continue
		}
		a.respond(m, src)
	}
}

func (a *advertiser) announceLoop() {
	defer a.wg.Done()
	t := time.NewTimer(a.every)
	defer t.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-a.kick:
			a.burst()
		case <-t.C:
			a.announce()
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(a.every)
	}
}

// burst is the RFC 6762 section 8.3 announcement: repeat, spaced out, because
// this is the moment a querier most needs to hear us and the moment our own
// radio is least likely to get a packet out.
func (a *advertiser) burst() {
	for i, d := range []time.Duration{0, 700 * time.Millisecond, 1500 * time.Millisecond} {
		if i > 0 {
			select {
			case <-a.stop:
				return
			case <-time.After(d):
			}
		}
		a.announce()
	}
}

func (a *advertiser) announce() {
	if a.addr() == nil {
		return
	}
	a.send(reply(a.fullSet(ttlDescribe, ttlLocate), nil), nil)
}

// goodbye withdraws everything by re-sending it with a zero TTL, so a querier
// drops us immediately instead of listing a device that is gone.
func (a *advertiser) goodbye() {
	if a.addr() == nil {
		return
	}
	a.send(reply(a.fullSet(0, 0), nil), nil)
}

// setAddr points the advertisement at a new address, or withdraws it when the
// address is nil. Unlike re-registering, this keeps our identity: the endpoint,
// instance and host name a querier already cached stay valid.
func (a *advertiser) setAddr(ip net.IP) {
	a.mu.Lock()
	old := a.ip
	if ip.Equal(old) && (ip == nil) == (old == nil) {
		a.mu.Unlock()
		return
	}
	a.ip = ip
	a.mu.Unlock()

	if ip == nil {
		// Announce the withdrawal using the address we still had a moment ago.
		a.mu.Lock()
		a.ip = old
		a.mu.Unlock()
		a.goodbye()
		a.mu.Lock()
		a.ip = nil
		a.mu.Unlock()
		return
	}
	a.refresh()
}

func (a *advertiser) close() {
	a.once.Do(func() {
		close(a.stop)
		a.goodbye()
		a.mu.Lock()
		conn := a.conn
		a.conn = nil
		a.mu.Unlock()
		if conn != nil {
			conn.Close()
		}
		a.wg.Wait()
	})
}

// hostLabel turns a display name into a DNS label. The endpoint id is appended
// because we do not implement probing, so a second Kindle on the same network
// claiming the same name is the one collision we can cheaply avoid.
func hostLabel(alias, epID string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(alias) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "kindshare"
	}
	return s + "-" + strings.ToLower(epID)
}
