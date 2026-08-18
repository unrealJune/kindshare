# Compatibility

## The short version

**The two halves have very different requirements.**

| | Quick Share receiver | SoftAP |
|---|---|---|
| Needs AP mode in the wifi driver | **no** | yes |
| Needs `hostapd` | **no** | yes |
| Needs Bluetooth | **no** | no |
| Needs root | for the firewall rule and port | yes |
| Works on a device that can only join networks | **yes** | no |

If your device can join a wifi network and run a static Go binary, the **Quick Share
receiver will almost certainly work**. It's an ordinary mDNS advertisement plus a TCP
listener. Everything exotic in this project is in the SoftAP half.

So on hardware without AP mode, the sensible setup is: join whatever network you already
have (home wifi, or your phone's hotspot) and run the receiver. You lose only the
no-router case.

## Verified

| | |
|---|---|
| Device | Kindle Voyage (board *Icewine*), i.MX6SL |
| Firmware | 5.13.6 |
| Kernel | 3.0.35-lab126, armv7l |
| Wi-Fi | Atheros AR6003 hw2.1.1, SDIO |
| Driver | **upstream `ath6kl_sdio`** (compat-wireless backport), v3.4.0.158 |
| Firmware blob | 3.4.0.225.3.SMARTPHONE, api 4 |
| Bluetooth | none — and none needed |

Both halves work on this device: Quick Share receiving over ordinary wifi *and* over the
Kindle's own SoftAP.

## Senders

| Sender | Status |
|---|---|
| Android Quick Share | verified |
| macOS, [NearDrop](https://github.com/grishka/NearDrop) | verified |
| Windows Quick Share app | expected to work; same discovery and protocol |
| ChromeOS Nearby Share | untested |

The senders differ in how hard they lean on mDNS being correct, and that is the only
place they have differed in practice. Android and Windows resolve a service by taking the
address out of whatever additional records arrive with the browse answer. Apple's resolver
follows the chain properly — SRV, then an `A` query for the target host — so a responder
that never answers address queries produces a device that appears in the share sheet and
then cannot be connected to. Our responder answers them; see `src/kindshare/mdns.go` for
what else it does that a general-purpose library did not.

## Checking your device

**Does the receiver stand a chance?** Almost certainly yes if it runs Linux and has a
network. Check that multicast works, since mDNS depends on it:

```sh
# should print packets from other devices on the LAN
kindshare -sniff -iface wlan0 -service _services._dns-sd._udp
```

**Does SoftAP stand a chance?**

```sh
iw phy phy0 info | sed -n '/Supported interface modes/,/^\t[A-Z]/p'
```

You need `* AP` in that list. On the Voyage:

```
Supported interface modes:
     * IBSS
     * managed
     * AP
interface combinations are not supported
```

`interface combinations are not supported` means the radio can't be a station **and** an
AP simultaneously — bringing up the AP takes the device off your network. That's why the
control surface has to live on the device (see [PORTING.md](PORTING.md)).

## Kindle notes

Other jailbroken Kindles are plausible but untested. What to check:

- **Driver.** The Voyage runs *upstream* `ath6kl_sdio`. Some Kindles ship Amazon's
  proprietary `ar6000.ko` instead, which exposes wireless extensions rather than nl80211
  and **does not advertise AP through cfg80211**. `lsmod | grep -iE 'ath6kl|ar6000'`
  tells you which you have. With `ar6000`, `iw` won't work at all and the SoftAP half
  would need a completely different approach; the receiver half is unaffected.
- **`iw` and `wpa_supplicant`** are present on FW 5.13.x. `hostapd` is not — you build it
  ([PORTING.md](PORTING.md#building-hostapd)).
- **`INPUT` policy is `DROP`.** Every listening port needs an explicit rule. This is the
  single most common cause of "it should work but doesn't" on these devices.
- **`/mnt/us` is a FUSE mount where everything is `0777`**, which trips OpenSSH
  `StrictModes` and dropbear's key checks.

## Kobo notes (untested)

Kobo is the most likely second target: KOReader is first-class there, and the plugin
should work as-is. Differences to expect:

- No upstart — Kobo uses its own init. The boot integration needs rewriting; the usual
  hook is `/mnt/onboard/.adds/koreader/...` or a `udev`/`on-startup` script.
- Paths differ: `/mnt/onboard` rather than `/mnt/us`. Both scripts and the plugin have
  these near the top.
- Kobos generally do **not** have a `DROP` firewall policy, so the iptables work may be
  unnecessary — harmless if `iptables` is absent, since the daemon logs and continues.
- Wifi chips vary widely; check AP support with the `iw` command above.

## Other Linux devices

The receiver is plain Go with no cgo. It has been built for `linux/arm` (GOARM=7) and
`windows/amd64`; anything Go targets should work. On a normal Linux box you likely need
nothing but the binary — no firewall rules, no root, if you pick a port above 1024.

Notably, **Go 1.26 runs fine on kernel 3.0.35**, well below what modern Rust supports.
That's why this is written in Go.
