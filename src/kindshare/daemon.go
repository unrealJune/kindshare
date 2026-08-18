package main

// Always-on supervisor.
//
// The naive design - start once, advertise once - breaks constantly in real
// use. mDNS publishes an A record for whatever address the interface had at
// registration time, and this Kindle's DHCP lease moved four times in a single
// evening. After any wifi toggle, reconnect or lease change the advertisement
// still points at a dead address: the phone lists the device, tries to connect,
// and fails. Restarting a crashed process would not help, because nothing
// crashes.
//
// So the daemon watches the interface and re-registers whenever the address
// changes. The TCP listener binds 0.0.0.0 once and is unaffected by any of it.

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"os/exec"

	"github.com/grandcat/zeroconf"
)

type daemonState struct {
	Mode        string `json:"mode"` // station | softap | down
	IP          string `json:"ip"`
	Advertising bool   `json:"advertising"`
	Alias       string `json:"alias"`
	DeviceID    string `json:"deviceId"`
	Port        int    `json:"port"`
	Received    int64  `json:"received"`
	LastFile    string `json:"lastFile"`
	LastError   string `json:"lastError"`
	Since       string `json:"since"`
}

var (
	filesReceived atomic.Int64
	lastFileName  atomic.Value // string
)

const statusPath = "/tmp/kindshare-status.json"

// currentIPv4 returns the interface's usable IPv4, ignoring link-local
// autoconfiguration addresses, which mean "no network" in practice.
func currentIPv4(ifname string) string {
	ni, err := net.InterfaceByName(ifname)
	if err != nil {
		return ""
	}
	if ni.Flags&net.FlagUp == 0 {
		return ""
	}
	addrs, err := ni.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		return ip.String()
	}
	return ""
}

// modeFor labels the address so the UI can say something useful.
func modeFor(ip string) string {
	switch {
	case ip == "":
		return "down"
	case ip == apAddress:
		return "softap"
	default:
		return "station"
	}
}

const apAddress = "192.168.55.1"

func writeStatus(s daemonState) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := statusPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	// Rename so a reader never sees a half-written file.
	os.Rename(tmp, statusPath)
}

// runDaemon owns the listener for the process lifetime and keeps the mDNS
// registration matched to the current address.
func runDaemon(id *identity, ifname string, port int, dtype byte, dest string, native bool, announceEvery time.Duration) {
	// One endpoint identity for the life of the process - and, now that it is
	// loaded from a file, for the life of the device. Regenerating it made the
	// device look like a different peer on every restart, which left the phone
	// holding stale entries that answer to nothing.
	alias := id.Display()
	epID := []byte(id.ID)
	instance := serviceInstanceName(epID)
	info := endpointInfo(alias, dtype, -1, false, id.DevID)

	log.Printf("kindshare daemon: alias=%q iface=%s port=%d dest=%s", alias, ifname, port, dest)
	log.Printf("  endpoint id %s, instance %s", string(epID), instance)
	if id.ephemeral {
		log.Printf("  WARNING: identity is ephemeral - it will change on restart")
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go acceptLoop(ln)

	lastFileName.Store("")

	var ifi *net.Interface
	if ni, err := net.InterfaceByName(ifname); err == nil {
		ifi = ni
	}

	// The native responder is the one that works with macOS; see mdns.go for
	// what the library it replaces does not do. zeroconf stays reachable behind
	// a flag so a regression can be confirmed on the device rather than argued
	// about.
	var adv *advertiser
	if native {
		adv = &advertiser{
			instance: instance,
			service:  serviceType,
			domain:   domain,
			// The base name, not the alias: hostLabel appends the id itself.
			host:  hostLabel(id.Name, string(epID)),
			port:  port,
			txt:   []string{"n=" + info},
			iface: ifi,
			every: announceEvery,
		}
		if err := adv.start(); err != nil {
			log.Fatalf("mdns: %v", err)
		}
		defer adv.close()
	}

	var (
		server  *zeroconf.Server
		curIP   string
		lastErr string
		since   = time.Now()
	)

	advertising := func() bool {
		if adv != nil {
			return adv.addr() != nil
		}
		return server != nil
	}

	republish := func(ip string, force bool) {
		// Re-assert our firewall rules every time. This device runs
		// `INPUT policy DROP`, and the rules have been removed out from under us
		// more than once (the SoftAP teardown used to delete them). Losing them
		// is invisible: we keep advertising happily while every incoming
		// connection is dropped before it reaches us.
		ensureFirewall(port)

		if adv != nil {
			if force {
				// The address can be unchanged while the interface underneath
				// it was rebuilt, so setAddr would decide there is nothing to
				// do. Force the socket and the announcement regardless.
				adv.refresh()
			}
			adv.setAddr(net.ParseIP(ip))
			if ip == "" {
				log.Printf("network down - advertisement withdrawn")
			} else {
				since = time.Now()
				lastErr = ""
				log.Printf("advertising %q on %s:%d as %s", alias, ip, port, adv.hostName())
			}
			return
		}

		if server != nil {
			server.Shutdown()
			server = nil
		}
		if ip == "" {
			log.Printf("network down - advertisement withdrawn")
			return
		}
		var ifaces []net.Interface
		if ni, err := net.InterfaceByName(ifname); err == nil {
			ifaces = []net.Interface{*ni}
		}
		s, err := zeroconf.Register(instance, serviceType, domain, port,
			[]string{"n=" + info}, ifaces)
		if err != nil {
			lastErr = err.Error()
			log.Printf("mDNS register failed on %s: %v (will retry)", ip, err)
			return
		}
		server = s
		lastErr = ""
		since = time.Now()
		log.Printf("advertising %q on %s:%d", alias, ip, port)
	}

	const pollInterval = 3 * time.Second
	lastTick := time.Now()
	tick := 0

	for {
		// Re-assert the firewall periodically, not only on state changes:
		// anything on the device can delete the rule at any moment, and the
		// symptom is indistinguishable from working correctly.
		if tick%20 == 0 {
			ensureFirewall(port)
		}
		tick++

		// Suspend detection by wall-clock jump. The Voyage suspends
		// aggressively and the whole process freezes with it; on resume the
		// address may be unchanged but our multicast group membership is often
		// stale, so the advertisement looks alive while being invisible. Any
		// gap much larger than the poll interval means we were asleep, and the
		// safe response is to republish unconditionally.
		now := time.Now()
		gap := now.Sub(lastTick)
		resumed := gap > pollInterval*4
		lastTick = now

		ip := currentIPv4(ifname)

		switch {
		case resumed && ip != "":
			log.Printf("resumed after %s asleep - re-registering on %s",
				gap.Round(time.Second), ip)
			curIP = ip
			republish(ip, true)

		case ip != curIP:
			// The case that matters in normal running: address changed under us.
			if curIP != "" && ip != "" {
				log.Printf("address changed %s -> %s, re-registering", curIP, ip)
			}
			curIP = ip
			republish(ip, false)

		case ip != "" && !advertising():
			// We have a network but no live registration - a previous attempt
			// failed. Keep retrying rather than sitting silently broken.
			republish(ip, true)
		}

		writeStatus(daemonState{
			Mode:        modeFor(curIP),
			IP:          curIP,
			Advertising: advertising(),
			Alias:       alias,
			DeviceID:    id.ID,
			Port:        port,
			Received:    filesReceived.Load(),
			LastFile:    lastFileName.Load().(string),
			LastError:   lastErr,
			Since:       since.Format(time.RFC3339),
		})

		time.Sleep(pollInterval)
	}
}

// ensureFirewall makes sure our ports are accepted. The Kindle's INPUT chain
// has a DROP policy, so a missing rule is not a degraded state - it is total,
// silent failure that looks identical to everything working.
func ensureFirewall(port int) {
	rules := [][]string{
		{"-p", "tcp", "--dport", fmt.Sprint(port), "-j", "ACCEPT"},
		{"-p", "udp", "--dport", "5353", "-j", "ACCEPT"},
	}
	for _, r := range rules {
		check := append([]string{"-C", "INPUT"}, r...)
		if err := exec.Command("iptables", check...).Run(); err == nil {
			continue // already present
		}
		insert := append([]string{"-I", "INPUT", "1"}, r...)
		if err := exec.Command("iptables", insert...).Run(); err != nil {
			log.Printf("firewall: could not add rule %v: %v", r, err)
		} else {
			log.Printf("firewall: re-added missing rule %v", r)
		}
	}
}
