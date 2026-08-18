package main

import (
	"log"
	"net"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// sniff joins the mDNS multicast group directly and logs every packet seen,
// optionally sending its own PTR query first.
//
// This exists to take the zeroconf library out of the picture. Browsing found
// nothing from either the laptop or the Kindle, even for service types that
// exist on any normal network, so the question is whether multicast reaches us
// at all. If packets appear here, the network is fine and the library is at
// fault; if nothing appears even while other devices are chattering, the link
// is dropping multicast and no amount of protocol work will help.
// qname/qtype let us interrogate one specific record instead of the service
// PTR. Comparing a working advertiser's TXT against our own is the only way to
// tell a malformed advertisement from a rejected one.
var (
	SniffQName string
	SniffQType uint16 = dns.TypePTR
)

func sniff(ifname, service string, query bool) {
	var ifi *net.Interface
	if ifname != "" {
		var err error
		ifi, err = net.InterfaceByName(ifname)
		if err != nil {
			log.Printf("sniff: interface %s: %v", ifname, err)
			return
		}
	}

	group := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	conn, err := net.ListenMulticastUDP("udp4", ifi, group)
	if err != nil {
		log.Printf("sniff: join 224.0.0.251:5353 on %s: %v", ifname, err)
		return
	}
	defer conn.Close()
	conn.SetReadBuffer(65536)
	log.Printf("sniff: listening on 224.0.0.251:5353 via %s", ifname)

	if query {
		go func() {
			for i := 0; i < 5; i++ {
				m := new(dns.Msg)
				name := service + "." + domain
				qt := dns.TypePTR
				if SniffQName != "" {
					name = SniffQName
					qt = SniffQType
				}
				m.SetQuestion(dns.Fqdn(name), qt)
				m.RecursionDesired = false
				b, err := m.Pack()
				if err != nil {
					log.Printf("sniff: pack: %v", err)
					return
				}
				// Pin the outgoing interface. A plain DialUDP hands the
				// choice to the routing table, which on a machine with a VPN
				// or a hypervisor bridge sends the query out of an adapter the
				// answering device is not on - and the failure looks exactly
				// like "nobody answered".
				out, err := net.ListenPacket("udp4", ":0")
				if err != nil {
					log.Printf("sniff: socket: %v", err)
					return
				}
				p := ipv4.NewPacketConn(out.(*net.UDPConn))
				var cm *ipv4.ControlMessage
				if ifi != nil {
					if err := p.SetMulticastInterface(ifi); err != nil {
						log.Printf("sniff: multicast interface %s: %v", ifi.Name, err)
					}
					cm = &ipv4.ControlMessage{IfIndex: ifi.Index}
				}
				p.SetMulticastTTL(255)
				if _, err := p.WriteTo(b, cm, group); err != nil {
					log.Printf("sniff: send query: %v", err)
				} else {
					log.Printf("sniff: sent %s query for %s (%d bytes) via %s",
						dns.TypeToString[qt], name, len(b), ifaceName(ifi))
				}
				p.Close()
				time.Sleep(3 * time.Second)
			}
		}()
	}

	buf := make([]byte, 65536)
	seen := 0
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("sniff: read: %v", err)
			return
		}
		seen++
		msg := new(dns.Msg)
		if err := msg.Unpack(buf[:n]); err != nil {
			log.Printf("sniff: pkt %d from %s: %d bytes (unparseable: %v)", seen, src, n, err)
			continue
		}
		log.Printf("sniff: pkt %d from %s: %d bytes, %d q / %d ans",
			seen, src, n, len(msg.Question), len(msg.Answer))
		for _, q := range msg.Question {
			log.Printf("    Q %s %s", dns.TypeToString[q.Qtype], q.Name)
		}
		for _, a := range msg.Answer {
			log.Printf("    A %s", a.String())
		}
		// Additionals matter as much as answers here: a resolver takes the
		// address from this section, so a response that omits it is a device
		// that lists but cannot be dialled.
		for _, e := range msg.Extra {
			log.Printf("    + %s", e.String())
		}
	}
}

func ifaceName(ifi *net.Interface) string {
	if ifi == nil {
		return "the default route"
	}
	return ifi.Name
}
