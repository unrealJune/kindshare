# Troubleshooting

Every entry here is a failure that actually happened, with the symptom that made it hard
to find. Most of them look identical from outside: *everything reports healthy and it
still doesn't work*.

Start with:

```sh
/mnt/us/kindler/kindshare-svc.sh status
/mnt/us/kindler/kindshare-svc.sh log
```

---

## The device appears on the phone, but connecting fails

**Almost always the firewall.** The Kindle runs `INPUT policy DROP`, so the phone's TCP
SYN is dropped before anything in userspace sees it. Nothing logs an error — the daemon
is listening happily and never learns a connection was attempted.

```sh
iptables -L INPUT -n | grep 12345      # nothing = blocked
```

`kindshare-svc.sh status` reports this directly as `port : 12345 (BLOCKED)`. The daemon
re-asserts its rules every 60s and on every re-registration, so this should self-heal
within a minute.

Historically this was caused by the SoftAP teardown deleting a rule it didn't own. It's
fixed, but if you add scripts of your own, **only delete rules you created**.

## The phone drops off wifi as soon as you open Quick Share

**Symptom:** you open the share sheet and the phone leaves the network — watch the wifi
icon in the status bar, it disappears or switches to mobile data. The Kindle then can't
be found, or a transfer that had started stalls. Nothing is wrong on the Kindle: it is
advertising correctly to a network the phone is no longer on.

**Workaround: turn on the phone's Wi-Fi hotspot, even though nothing will connect to
it.** That's enough to stop Quick Share tearing down the wifi connection. Leave it on for
the transfer; you can switch it off afterwards.

This is a known Android/Quick Share bug, not something this software can fix. The
underlying cause is that Quick Share negotiates a peer-to-peer medium (Wi-Fi Direct or a
local-only hotspot) and a phone with a single radio can't hold an infrastructure
connection while bringing up a P2P group on another channel — so it drops the one it has.
Having a hotspot already up appears to keep it from doing that.

Worth checking first if the Kindle "sometimes" isn't found, because the failure looks
identical to a discovery problem.

## The device doesn't appear on the phone at all

Check, in order:

1. **Is the service running and advertising?**
   `kindshare-svc.sh status` → `advertising: true` and a sensible `ip`.
2. **Is the advertised address current?** If `ip` in the status file doesn't match
   `ifconfig wlan0`, re-registration failed — see the log for `mDNS register failed`.
3. **Does multicast work at all?**
   ```sh
   kindshare -sniff -iface wlan0 -service _services._dns-sd._udp
   ```
   Silence here means the network is blocking multicast (common on guest wifi) and mDNS
   cannot work regardless of what this software does.
4. **Local network permission on Android.** Newer Android versions gate access to LAN
   devices; if Quick Share hasn't been granted it, it will never see the Kindle.
5. **Restart the phone's share sheet.** Discovery genuinely does fail transiently.

**Do not trust `grandcat/zeroconf`'s Browse** for diagnosing this — it returns nothing on
this device even when raw multicast is fine. Use `-sniff`, which talks to the socket
directly. Advertising via the same library works correctly.

## Files arrive but the phone never says "sent"

The sender waits for acknowledgement. Two frames are required after the last chunk:

- a `PAYLOAD_TRANSFER` with `PacketType=PAYLOAD_ACK` (bare header, no `ControlMessage`)
- a `DISCONNECTION` frame once every introduced file has arrived

Without them the transfer completes on disk and the phone spins indefinitely, sending
keep-alives. This is implemented; it's here because it's non-obvious and absent from the
published protocol notes.

## SoftAP: clients associate but get no IP

**Do not run an open network on AR6003.** On an open BSS the firmware never authorizes
the station: it accepts frames *from* the client while dropping everything sent *to* it.
`udhcpd` logs `sending OFFER` and the client never receives it. hostapd's log shows the
driver being told `authorized=0`, reaching `authorized=1` only on a later re-association,
long after the client gave up.

WPA2-PSK replaces that implicit path with a 4-way handshake ending in an explicit
authorize, and the data path opens. This is why `device/hostapd.conf` sets `wpa=2` — for
the authorization, not the secrecy.

## SoftAP: clients associate then get dropped after seconds

Advertising capabilities the firmware can't honour. This driver rejects WMM/TX-queue
configuration outright:

```
nl80211: TX queue param set: queue=0 ... --> res=-95      (EOPNOTSUPP)
Failed to set TX queue parameters for queue 0.
```

so `ieee80211n=1` / `wmm_enabled=1` produce beacons carrying HT and WMM elements the
firmware can't back. Keep `ieee80211n=0` and `wmm_enabled=0`. Throughput is irrelevant
here — a book is a few MB.

## `iw dev wlan0 station dump` shows nothing with clients connected

This driver doesn't implement `dump_station` in AP mode. It returns empty **always**.

Never build client detection on it. Count hostapd's `AP-STA-CONNECTED` /
`AP-STA-DISCONNECTED` events instead. An early version of the dead-man timer used
`station dump` and would have torn down the AP during an active transfer.

## The service seems to start but nothing is listening

Something else already holds the port. A second copy exits immediately with
`bind: address already in use`, and unless you read the log it looks like it started.

```sh
pidof kindshare        # more than one, or an unexpected one
```

`kindshare-svc.sh start` refuses to start a duplicate, and the boot job checks too. If
you launch the binary by hand for testing, stop it before using the service scripts.

## SSH key auth fails on the device

Two device-specific traps:

- `/mnt/us` is a FUSE mount where everything is `0777`, which trips OpenSSH's
  `StrictModes`. Set `StrictModes no` in the usbnet `sshd_config`.
- usbnet's `authorized_keys` may exist as a **directory** of `.pub` files. The patched
  sshd wants a regular file and rejects a directory with
  `authorized keys ... is not a regular file` — silently, as an auth failure. Concatenate
  them into a file.

## Getting real diagnostics

When the AP is up you have no network path to the device, so collect on-device:

**Plugin → Save diagnostics to /mnt/us** writes a timestamped bundle plus the raw logs to
`/mnt/us/kindler/logs/`, which survives reboots — unlike `/tmp`, which is tmpfs.

`kindle-ap.sh` also samples state every 3 seconds into `/tmp/ap-samples.log` while the AP
is up: hostapd connection counts, EAPOL progress, DHCP events, and `/proc/net/dev`
counters. That timeline is usually what identifies the failure, because it covers the
window in which nothing can be observed live.

## A Mac sees the Kindle once, then can't reach it

Symptom: the Kindle shows up in NearDrop's device list occasionally, never reliably, and
picking it does nothing. Android on the same network is fine.

This was a real bug, fixed in the responder. If you are running an older build, or you
have ported this to another mDNS library, check it directly. Ask for the address of the
name our SRV record points at:

```sh
# from another machine on the same network
kindshare -sniff -iface wlan0 -qname kindle-voyage-xxxx.local. -qtype A
```

Get the exact name from the daemon log line `mdns: <instance> -> <host>:<port>`. A working
responder answers within milliseconds:

```
A kindle-voyage-xxxx.local.  120  CLASS32769  A  192.168.1.228
```

Silence there, while a browse for `_FC9F5ED42C8A._tcp.local.` is answered normally, is
exactly the failure: the service is discoverable and unresolvable. Note that the sniffer
now pins its outgoing interface — an earlier version let the routing table choose, which
on a machine with a VPN or a hypervisor bridge sent the query out of an adapter the Kindle
was not on, and looked identical to "nobody answered".
