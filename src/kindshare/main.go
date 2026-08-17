// kindshare - Quick Share (Nearby Share) receiver for the Kindle Voyage.
//
// STAGE 1: discovery only.
//
// The whole feature rests on one unproven assumption - that an Android phone
// will run mDNS discovery over a network it just joined, on an AP with no
// internet and no other hosts. If the Kindle never appears in the Quick Share
// sheet, no amount of UKEY2 work matters. So this binary advertises the service
// and accepts TCP connections, logging everything, and does not yet implement
// the protocol. It answers "are we discoverable, and does the phone dial us?"
//
// Protocol reference: github.com/grishka/NearDrop/blob/master/PROTOCOL.md
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grandcat/zeroconf"
	"google.golang.org/protobuf/proto"

	"kindshare/pb/connections"
)

const (
	// SHA256("NearbySharing"), first bytes, as used by the service type.
	serviceType = "_FC9F5ED42C8A._tcp"
	domain      = "local."

	deviceTypeUnknown = 0
	deviceTypePhone   = 1
	deviceTypeTablet  = 2
	deviceTypeLaptop  = 3
)

// endpointID is 4 random alphanumeric characters identifying this endpoint.
func endpointID() []byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return b
}

// serviceInstanceName builds the 10-byte instance name:
//
//	0x23 | endpointID[4] | 0xFC 0x9F 0x5E | 0x00 0x00
//
// then URL-safe base64 without padding.
func serviceInstanceName(epID []byte) string {
	b := make([]byte, 0, 10)
	b = append(b, 0x23) // PCP marker
	b = append(b, epID...)
	b = append(b, 0xFC, 0x9F, 0x5E) // service ID hash
	b = append(b, 0x00, 0x00)
	return base64.RawURLEncoding.EncodeToString(b)
}

// endpointInfo builds the TXT "n" value:
//
//	byte0: deviceType<<1  (version 0, visible, reserved bit clear)
//	[1:17]: 16 random bytes - salt + encrypted metadata key
//	then a 1-byte length prefix and the UTF-8 device name.
//
// byte0Override and omitName exist to test against a real advertiser observed on
// the wire, whose byte 0 is 0x16 - i.e. bit 4 SET. NearDrop's notes say bit 4 is
// visibility with 0 meaning visible, but a working Quick Share device sets it,
// so the documented polarity is suspect.
func endpointInfo(name string, deviceType byte, byte0Override int, omitName bool) string {
	b := make([]byte, 0, 1+16+1+len(name))
	if byte0Override >= 0 {
		b = append(b, byte(byte0Override))
	} else {
		b = append(b, (deviceType&0x07)<<1)
	}

	devID := make([]byte, 16)
	if _, err := rand.Read(devID); err != nil {
		panic(err)
	}
	b = append(b, devID...)

	if !omitName {
		n := []byte(name)
		if len(n) > 255 {
			n = n[:255]
		}
		b = append(b, byte(len(n)))
		b = append(b, n...)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// destDir is set from the -dest flag and read by the session handler.
var destDir *string

func main() {
	var (
		name      = flag.String("name", "Kindle Voyage", "device name shown on the phone")
		port      = flag.Int("port", 12345, "TCP port to advertise and listen on")
		iface     = flag.String("iface", "wlan0", "interface to advertise on (empty = all)")
		dtype     = flag.Int("devicetype", deviceTypeLaptop, "0 unknown 1 phone 2 tablet 3 laptop")
		browse    = flag.Bool("browse", false, "also browse for other Quick Share advertisers")
		advertise = flag.Bool("advertise", true, "advertise ourselves (set false for a pure checker)")
		svc       = flag.String("service", serviceType, "service type to browse for")
		doSniff   = flag.Bool("sniff", false, "raw mDNS sniffer: join 224.0.0.251:5353 and log every packet")
		doQuery   = flag.Bool("query", true, "in sniff mode, also send PTR queries")
		qname     = flag.String("qname", "", "sniff mode: query this exact record name")
		qtype     = flag.String("qtype", "PTR", "sniff mode: record type (PTR/TXT/SRV/ANY)")
		byte0     = flag.Int("byte0", -1, "override endpoint-info byte 0 (e.g. 0x16); -1 = compute")
		noName    = flag.Bool("noname", false, "omit the device-name field entirely")
		dest      = flag.String("dest", "/mnt/us/documents", "where received files are written")
		daemon    = flag.Bool("daemon", false, "run as a network-aware always-on receiver")
	)
	flag.Parse()
	destDir = dest

	// Role inversion probe. Normally the receiver advertises and the phone
	// discovers - that is the direction that failed. In Google's QR-code flow
	// the SENDER advertises and the scanner connects, so if the phone advertises
	// over mDNS while its Quick Share sheet is open, we can discover IT. The
	// Kindle discovering is an ordinary client operation, unlike being
	// discovered, so this is the direction most likely to work.
	if *doSniff {
		SniffQName = *qname
		if v, ok := dnsTypeByName(*qtype); ok {
			SniffQType = v
		}
		sniff(*iface, *svc, *doQuery)
		return
	}

	if *browse {
		go browseForPeers(*iface, *svc)
	}

	// Daemon mode owns its own lifecycle: it re-registers when the address
	// changes, which the one-shot path below cannot do.
	if *daemon {
		runDaemon(*name, *iface, *port, byte(*dtype), *dest)
		return
	}

	epID := endpointID()
	instance := serviceInstanceName(epID)
	info := endpointInfo(*name, byte(*dtype), *byte0, *noName)

	log.Printf("kindshare stage-1 discovery probe")
	log.Printf("  endpoint id : %s", string(epID))
	log.Printf("  instance    : %s", instance)
	log.Printf("  txt n=      : %s", info)
	log.Printf("  service     : %s.%s", serviceType, domain)
	log.Printf("  port        : %d", *port)

	var ifaces []net.Interface
	if *iface != "" {
		ni, err := net.InterfaceByName(*iface)
		if err != nil {
			log.Printf("  WARNING: interface %s not found (%v), advertising on all", *iface, err)
		} else {
			ifaces = []net.Interface{*ni}
			log.Printf("  iface       : %s", ni.Name)
		}
	}

	// Browse-only mode: a pure checker, used to verify from another machine
	// that our advertisement is actually visible on the wire.
	if !*advertise {
		log.Printf("browse-only mode - not advertising, just watching")
		select {}
	}

	// Listen first: if the phone dials us the instant it sees the advert, we
	// want the socket already up.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	server, err := zeroconf.Register(instance, serviceType, domain, *port,
		[]string{"n=" + info}, ifaces)
	if err != nil {
		log.Fatalf("mDNS register: %v", err)
	}
	defer server.Shutdown()
	log.Printf("advertising - open Quick Share on the phone and look for %q", *name)

	go acceptLoop(ln)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
}

// browseForPeers watches the Quick Share service type and logs anything that
// advertises, decoding the TXT endpoint info so we can see the peer's device
// name and type. Anything appearing here while the phone's share sheet is open
// means the inverted (phone-advertises) direction is reachable from the Kindle.
func browseForPeers(ifname, svc string) {
	var ifaces []net.Interface
	if ifname != "" {
		if ni, err := net.InterfaceByName(ifname); err == nil {
			ifaces = []net.Interface{*ni}
		}
	}
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIfaces(ifaces))
	if err != nil {
		log.Printf("browsing for %s%s", svc, domain)
		log.Printf("browse: resolver: %v", err)
		return
	}
	entries := make(chan *zeroconf.ServiceEntry, 8)
	go func() {
		for e := range entries {
			log.Printf("PEER FOUND: instance=%q host=%s port=%d addrs=%v",
				e.Instance, e.HostName, e.Port, e.AddrIPv4)
			for _, t := range e.Text {
				log.Printf("   txt: %s", t)
				if len(t) > 2 && t[:2] == "n=" {
					if raw, err := base64.RawURLEncoding.DecodeString(t[2:]); err == nil {
						describeEndpointInfo(raw)
					}
				}
			}
		}
	}()
	// Browse indefinitely.
	if err := resolver.Browse(context.Background(), svc, domain, entries); err != nil {
		log.Printf("browse: %v", err)
	}
}

// describeEndpointInfo decodes the TXT "n" blob back into readable fields.
func describeEndpointInfo(b []byte) {
	if len(b) < 18 {
		log.Printf("   endpoint info too short (%d bytes)", len(b))
		return
	}
	devType := (b[0] >> 1) & 0x07
	nameLen := int(b[17])
	peer := ""
	if len(b) >= 18+nameLen {
		peer = string(b[18 : 18+nameLen])
	}
	log.Printf("   decoded: deviceType=%d name=%q", devType, peer)
}

func acceptLoop(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go handle(c)
	}
}

// handle drives the plaintext part of the connection: the peer's
// ConnectionRequest, then the UKEY2 handshake.
func handle(c net.Conn) {
	defer c.Close()
	log.Printf("CONNECTION from %s", c.RemoteAddr())
	c.SetDeadline(time.Now().Add(2 * time.Minute))

	// --- 1. ConnectionRequest --------------------------------------------
	raw, err := readFrame(c)
	if err != nil {
		log.Printf("  read ConnectionRequest: %v", err)
		return
	}
	var off connections.OfflineFrame
	if err := proto.Unmarshal(raw, &off); err != nil {
		log.Printf("  parse OfflineFrame: %v (%d bytes: %x)", err, len(raw), head(raw))
		return
	}
	if req := off.GetV1().GetConnectionRequest(); req != nil {
		log.Printf("  ConnectionRequest: endpoint_id=%q name=%q",
			req.GetEndpointId(), endpointName(req.GetEndpointInfo()))
	} else {
		log.Printf("  first frame was not a ConnectionRequest: %v", off.GetV1().GetType())
	}

	// --- 2. UKEY2 ---------------------------------------------------------
	clientInit, err := readFrame(c)
	if err != nil {
		log.Printf("  read ClientInit: %v", err)
		return
	}
	keys, err := doHandshake(c, clientInit)
	if err != nil {
		log.Printf("  HANDSHAKE FAILED: %v", err)
		return
	}
	log.Printf("  HANDSHAKE OK")
	log.Printf("    auth string : %x", keys.authString[:8])
	log.Printf("    decrypt key : %x…", keys.decryptKey[:4])
	log.Printf("    encrypt key : %x…", keys.encryptKey[:4])

	// --- 3. our ConnectionResponse (plaintext) -----------------------------
	// The phone has already sent its own and is sitting in a keep-alive loop
	// waiting for ours. Encryption only begins once both sides have accepted.
	if err := sendConnectionResponse(c); err != nil {
		log.Printf("  send ConnectionResponse: %v", err)
		return
	}
	log.Printf("  sent ConnectionResponse (ACCEPT)")

	// --- 4. run the sharing state machine ----------------------------------
	sess := newSession(c, keys, *destDir)
	for {
		// Each frame resets the clock: a large file legitimately takes minutes.
		c.SetDeadline(time.Now().Add(2 * time.Minute))

		raw, err := readFrame(c)
		if err != nil {
			log.Printf("  read: %v", err)
			return
		}

		// Keep-alives and the peer's own ConnectionResponse can still arrive as
		// plaintext, so fall back rather than treating that as an error.
		inner, seq, derr := keys.decrypt(raw)
		if derr != nil {
			var pf connections.OfflineFrame
			if err := proto.Unmarshal(raw, &pf); err == nil && pf.GetV1() != nil {
				log.Printf("  plaintext frame: type=%v", pf.GetV1().GetType())
				continue
			}
			log.Printf("  undecodable frame (%v): %d bytes: %x", derr, len(raw), head(raw))
			continue
		}

		var f connections.OfflineFrame
		if err := proto.Unmarshal(inner, &f); err != nil {
			log.Printf("  DECRYPTED seq=%d but unparseable: %v", seq, err)
			continue
		}
		log.Printf("  DECRYPTED seq=%d type=%v (%d bytes)", seq, f.GetV1().GetType(), len(inner))
		describeFrame(&f)

		if err := sess.handleOffline(&f); err != nil {
			log.Printf("  session ended: %v", err)
			return
		}
	}
}

// sendConnectionResponse accepts the incoming connection.
func sendConnectionResponse(w io.Writer) error {
	b, err := proto.Marshal(&connections.OfflineFrame{
		Version: connections.OfflineFrame_V1.Enum(),
		V1: &connections.V1Frame{
			Type: connections.V1Frame_CONNECTION_RESPONSE.Enum(),
			ConnectionResponse: &connections.ConnectionResponseFrame{
				Response: connections.ConnectionResponseFrame_ACCEPT.Enum(),
				Status:   proto.Int32(0),
			},
		},
	})
	if err != nil {
		return err
	}
	return writeFrame(w, b)
}

// describeFrame logs the interesting contents of a decrypted frame so we can
// follow the PairedKey -> Introduction -> Response -> PayloadTransfer sequence.
func describeFrame(f *connections.OfflineFrame) {
	v1 := f.GetV1()
	if pt := v1.GetPayloadTransfer(); pt != nil {
		h := pt.GetPayloadHeader()
		log.Printf("     payload id=%d type=%v size=%d chunkOffset=%d",
			h.GetId(), h.GetType(), h.GetTotalSize(), pt.GetPayloadChunk().GetOffset())
	}
}

func head(b []byte) []byte {
	if len(b) > 64 {
		return b[:64]
	}
	return b
}

// endpointName pulls the human-readable name out of a peer's endpoint info:
// 1 flags byte, 16 bytes of salt+key, then a length-prefixed UTF-8 name.
func endpointName(b []byte) string {
	if len(b) < 18 {
		return ""
	}
	n := int(b[17])
	if len(b) < 18+n {
		return ""
	}
	return string(b[18 : 18+n])
}
