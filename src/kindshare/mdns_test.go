package main

import (
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func testAdvertiser() *advertiser {
	a := &advertiser{
		instance: "IzRhYmP8n14AAA",
		service:  serviceType,
		domain:   domain,
		host:     hostLabel("Kindle Voyage", "aB3z"),
		port:     12345,
		txt:      []string{"n=" + endpointInfo("Kindle Voyage", deviceTypeLaptop, -1, false)},
	}
	a.ip = net.IPv4(192, 168, 1, 228)
	return a
}

func ask(t *testing.T, a *advertiser, name string, qtype uint16) ([]dns.RR, []dns.RR) {
	t.Helper()
	return a.answersFor(dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET})
}

func hasType(rrs []dns.RR, t uint16) dns.RR {
	for _, rr := range rrs {
		if rr.Header().Rrtype == t {
			return rr
		}
	}
	return nil
}

// The regression this file exists for: a querier that has resolved the service
// and now wants the address of the SRV target must get an answer. The library
// we replaced matched only three names and this was not one of them, so macOS
// listed the device and then could not reach it.
func TestAnswersAddressQueryForHostName(t *testing.T) {
	a := testAdvertiser()

	for _, qtype := range []uint16{dns.TypeA, dns.TypeANY} {
		ans, _ := ask(t, a, a.hostName(), qtype)
		rr := hasType(ans, dns.TypeA)
		if rr == nil {
			t.Fatalf("no A record for %s (qtype %s)", a.hostName(), dns.TypeToString[qtype])
		}
		if got := rr.(*dns.A).A.String(); got != "192.168.1.228" {
			t.Fatalf("A record = %s, want 192.168.1.228", got)
		}
		if rr.Header().Class&cacheFlush == 0 {
			t.Errorf("address record should carry the cache-flush bit")
		}
		if rr.Header().Ttl != ttlLocate {
			t.Errorf("A ttl = %d, want %d", rr.Header().Ttl, ttlLocate)
		}
	}
}

// The SRV target has to be a name we actually answer for, or the lookup above
// is asked of someone else and nobody replies.
func TestSRVTargetIsOurHostName(t *testing.T) {
	a := testAdvertiser()
	ans, extra := ask(t, a, a.instanceName(), dns.TypeSRV)
	srv, _ := hasType(ans, dns.TypeSRV).(*dns.SRV)
	if srv == nil {
		t.Fatal("no SRV answer for the instance name")
	}
	if srv.Target != a.hostName() {
		t.Fatalf("SRV target = %q, want %q", srv.Target, a.hostName())
	}
	if srv.Port != 12345 {
		t.Fatalf("SRV port = %d, want 12345", srv.Port)
	}
	if hasType(extra, dns.TypeA) == nil {
		t.Error("SRV answer should carry the address alongside it")
	}
}

func TestBrowseAnswerCarriesEverythingNeeded(t *testing.T) {
	a := testAdvertiser()
	ans, extra := ask(t, a, a.serviceName(), dns.TypePTR)

	ptr, _ := hasType(ans, dns.TypePTR).(*dns.PTR)
	if ptr == nil {
		t.Fatal("no PTR answer for the service name")
	}
	if ptr.Ptr != a.instanceName() {
		t.Fatalf("PTR = %q, want %q", ptr.Ptr, a.instanceName())
	}
	// Shared record: claiming it exclusively would evict every other device
	// advertising the same service type from the querier's cache.
	if ptr.Hdr.Class&cacheFlush != 0 {
		t.Error("the service PTR must not set the cache-flush bit")
	}
	for _, want := range []uint16{dns.TypeSRV, dns.TypeTXT, dns.TypeA} {
		if hasType(extra, want) == nil {
			t.Errorf("browse answer is missing a %s record", dns.TypeToString[want])
		}
	}
}

func TestTXTAndEnumerationAndUnknownNames(t *testing.T) {
	a := testAdvertiser()

	ans, _ := ask(t, a, a.instanceName(), dns.TypeTXT)
	txt, _ := hasType(ans, dns.TypeTXT).(*dns.TXT)
	if txt == nil {
		t.Fatal("no TXT answer for the instance name")
	}
	if len(txt.Txt) != 1 || !strings.HasPrefix(txt.Txt[0], "n=") {
		t.Fatalf("TXT = %v, want a single n= entry", txt.Txt)
	}

	if ans, _ := ask(t, a, a.enumName(), dns.TypePTR); hasType(ans, dns.TypePTR) == nil {
		t.Error("no answer to the service-type enumeration")
	}

	// Case is not significant in DNS and queriers do vary it.
	if ans, _ := ask(t, a, strings.ToUpper(a.hostName()), dns.TypeA); hasType(ans, dns.TypeA) == nil {
		t.Error("host name matching should be case-insensitive")
	}

	for _, name := range []string{"somebody-else.local.", "_http._tcp.local."} {
		if ans, _ := ask(t, a, name, dns.TypeANY); len(ans) != 0 {
			t.Errorf("answered for %q, which is not ours: %v", name, ans)
		}
	}
	if ans, _ := ask(t, a, a.hostName(), dns.TypeTXT); len(ans) != 0 {
		t.Error("a TXT query for the host name should go unanswered")
	}
}

// With no address we answer nothing rather than advertising a dead host.
func TestNoAddressMeansNoAnswer(t *testing.T) {
	a := testAdvertiser()
	a.ip = nil

	if ans, _ := ask(t, a, a.hostName(), dns.TypeA); len(ans) != 0 {
		t.Errorf("answered an address query with no address: %v", ans)
	}
	ans, extra := ask(t, a, a.serviceName(), dns.TypePTR)
	if len(ans) == 0 {
		t.Error("the service should still be listed while its address is unknown")
	}
	if hasType(extra, dns.TypeA) != nil {
		t.Error("attached an address record while having no address")
	}
	if rrs := a.fullSet(ttlDescribe, ttlLocate); hasType(rrs, dns.TypeA) != nil {
		t.Error("announcement carried an address record while having no address")
	}
}

// Every message we build has to survive the wire format: an unterminated name
// or an over-long string fails at Pack time, which at runtime would only show
// up as a log line and a device nobody can see.
func TestAnnouncementPacks(t *testing.T) {
	a := testAdvertiser()
	m := reply(a.fullSet(ttlDescribe, ttlLocate), nil)
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("pack announcement: %v", err)
	}

	var back dns.Msg
	if err := back.Unpack(b); err != nil {
		t.Fatalf("unpack announcement: %v", err)
	}
	if !back.Response || !back.Authoritative {
		t.Error("announcement must be an authoritative response")
	}
	// RFC 6762 section 6: zero id, no echoed questions.
	if back.Id != 0 {
		t.Errorf("message id = %d, want 0", back.Id)
	}
	if len(back.Question) != 0 {
		t.Errorf("response carries %d questions, want 0", len(back.Question))
	}
	if len(back.Answer) != len(m.Answer) {
		t.Errorf("round trip lost records: %d -> %d", len(m.Answer), len(back.Answer))
	}

	// A goodbye is the same set at zero TTL.
	for _, rr := range a.fullSet(0, 0) {
		if rr.Header().Ttl != 0 {
			t.Errorf("goodbye record %s has ttl %d", rr.Header().Name, rr.Header().Ttl)
		}
	}
}

func TestHostLabel(t *testing.T) {
	for _, tc := range []struct{ alias, want string }{
		{"Kindle Voyage", "kindle-voyage-ab3z"},
		{"june's Kindle!", "junes-kindle-ab3z"},
		{"", "kindshare-ab3z"},
		{"...", "kindshare-ab3z"},
	} {
		if got := hostLabel(tc.alias, "aB3z"); got != tc.want {
			t.Errorf("hostLabel(%q) = %q, want %q", tc.alias, got, tc.want)
		}
	}
	// Whatever comes out has to be a legal DNS label.
	long := hostLabel(strings.Repeat("x", 100), "aB3z")
	if len(long) > 63 {
		t.Errorf("label is %d bytes, over the 63-byte limit: %q", len(long), long)
	}
	if _, ok := dns.IsDomainName(long + ".local."); !ok {
		t.Errorf("%q is not a usable domain name", long)
	}
}
