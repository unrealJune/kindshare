# kindshare

Receive files on a jailbroken Kindle straight from an Android phone's **Quick Share**
(Nearby Share) — no cable, no router required, no cloud, nothing to install on the phone.

It also turns the Kindle into a **Wi-Fi access point** for when there's no network to
share, so the phone can connect directly to the Kindle.

Not only Android: any Quick Share sender works. Sending from macOS with
[NearDrop](https://github.com/grishka/NearDrop) is verified too.

Files land in `/mnt/us/documents`, so an epub sent this way appears in your library.

**It ships as a KOReader plugin.** Everything is driven from
KOReader → **Network** → **Quick Share / SoftAP**: turn receiving on or off, enable it at
boot, start the access point, and read diagnostics — all on the device, no shell needed
once it's installed.

```
Quick Share / SoftAP
  Receiving: ON  (tap to stop)
  Start receiving at boot            [x]
  Service status
  Start access point
  Stop access point, restore Wi-Fi
  Keep access point up (disarm timer)
  Diagnostics
  Save diagnostics to /mnt/us
```

The plugin is the control surface only — the receiver itself runs as a small background
daemon, so transfers keep working when KOReader isn't open. See
[How it works](#how-it-works).

Verified end to end on a **Kindle Voyage, firmware 5.13.6** (kernel 3.0.35-lab126,
Atheros AR6003 / `ath6kl_sdio`). See [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md)
before assuming it works on your device — and note that the **Quick Share half needs no
access-point support at all**, which makes it portable to far more hardware than the
SoftAP half.

---

## What's in here

| Path | What it is |
|---|---|
| `plugin/kindleap.koplugin/` | KOReader plugin — the on-device UI |
| `src/kindshare/` | The Quick Share receiver daemon (Go) |
| `src/kindrop/` | A plain HTTP upload page, as a fallback receiver |
| `device/` | Scripts and configs installed on the device |
| `proto/` | Protobuf definitions for the Nearby Share protocol |
| `build/` | Dockerfile that cross-compiles `hostapd` for armv7 |
| `docs/` | Compatibility, porting, protocol notes, troubleshooting |

## Requirements

- A **jailbroken** Kindle with **KUAL** and **KOReader**
- SSH access (the [usbnetwork hack](https://www.mobileread.com/forums/showthread.php?t=186645) is the usual route)
- A Linux/macOS/Windows machine with Go and Docker to build

Quick Share itself needs **nothing installed on the phone** — it's the sharing feature
Android already ships.

## Install

```sh
# 1. Build the receiver (static, no libc dependency on the device)
cd src/kindshare
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o kindshare .

# 2. Copy everything onto the Kindle
scp kindshare            root@KINDLE:/mnt/us/kindler/bin/
scp ../../device/*.sh    root@KINDLE:/mnt/us/kindler/
scp ../../device/*.conf  root@KINDLE:/mnt/us/kindler/etc/
scp -r ../../plugin/kindleap.koplugin root@KINDLE:/mnt/us/koreader/plugins/

# 3. On the device
chmod +x /mnt/us/kindler/*.sh /mnt/us/kindler/bin/*
/mnt/us/kindler/kindshare-svc.sh start
```

For SoftAP you also need `hostapd` — see [docs/PORTING.md](docs/PORTING.md#building-hostapd).

**Scripts must have UNIX line endings.** If you edit them on Windows, run
`sed -i 's/\r$//' *.sh` on the device, or the shell fails in confusing ways.

### Start at boot (optional)

```sh
mntroot rw
cp /mnt/us/kindler/kindshare.conf /etc/upstart/kindshare.conf
mntroot ro
/mnt/us/kindler/kindshare-svc.sh enable
```

This is the **only** write to the root filesystem. After it, enabling and disabling
autostart only creates or deletes `/mnt/us/kindler/autostart` — the upstart job checks
for that flag and exits if it's missing, so you never remount root again.

## KOReader plugin setup

### Installing

Copy the plugin folder into KOReader's `plugins` directory and restart KOReader —
plugins are only loaded at startup, so it won't appear until you do:

```sh
scp -r plugin/kindleap.koplugin root@KINDLE:/mnt/us/koreader/plugins/
```

If you edited anything on Windows, strip carriage returns or LuaJIT will fail to parse
the file:

```sh
sed -i 's/\r$//' /mnt/us/koreader/plugins/kindleap.koplugin/*.lua
```

Check it parses before restarting — a syntax error makes the plugin vanish silently
rather than complain:

```sh
/mnt/us/koreader/luajit -e 'print(loadfile("/mnt/us/koreader/plugins/kindleap.koplugin/main.lua") and "OK" or "ERROR")'
```

It then appears under **Network** in KOReader's top menu as **Quick Share / SoftAP**.

### The controls

| Entry | What it does |
|---|---|
| **Receiving: ON / off** | Starts or stops the receiver **right now**. The label reflects the real process state, not a remembered setting. |
| **Start receiving at boot** ☐ | Whether the receiver starts automatically after a reboot. Needs the upstart job installed once — see [Start at boot](#start-at-boot-optional). |
| **Service status** | Mode (`station` / `softap` / `down`), current address, whether the port is genuinely open, files received, last error. |
| **Start access point** | Turns the Kindle into an AP. Confirms first, because wlan0 leaves station mode and any SSH session drops. |
| **Stop access point, restore Wi-Fi** | Tears the AP down and restarts normal wifi. |
| **Keep access point up (disarm timer)** | Cancels the automatic revert (below). |
| **Diagnostics** | Everything about the current state on screen: interfaces, processes, firewall counters, recent logs. |
| **Save diagnostics to /mnt/us** | Writes a timestamped bundle plus raw logs to `/mnt/us/kindler/logs/`, which survives a reboot. Use this when the AP is up and nothing can reach the device. |

**The two toggles are independent.** Stopping the service for now doesn't change whether
it starts at boot, and vice versa — so you can silence it for an afternoon without
forgetting to re-enable it later.

**The access point reverts itself.** If no client connects within ten minutes it tears
the AP down and restores wifi automatically, so a failed attempt costs a wait rather than
a walk to the device. The countdown measures *idle* time and resets while a client is
connected, so it can't interrupt a transfer in progress. **Keep access point up** disarms
it if you want the AP to persist.

## Use it

On the phone: share a file, pick **Kindle Voyage**, done.

With **SoftAP**, join the Kindle's network from the phone first:

| | |
|---|---|
| **Network (SSID)** | `kindrop` |
| **Password** | `kindlevoyage` |
| Security | WPA2-PSK (CCMP) |
| The Kindle's address | `192.168.55.1` |

Quick Share keeps working over it — the daemon follows the address change automatically,
so there is nothing extra to switch on.

**Change the password.** It's a published default, so anyone reading this page knows it.
Edit `wpa_passphrase` (and `ssid` if you like) in `device/hostapd.conf`, copy it to
`/mnt/us/kindler/etc/hostapd.conf`, and restart the access point.

The AP runs WPA2 rather than open **by necessity, not for privacy** — on an open network
this wifi firmware never authorizes the client and no data flows at all. See
[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md#softap-clients-associate-but-get-no-ip).

> **If the Kindle isn't found, check the phone's wifi icon.** On some Android builds,
> opening Quick Share makes the phone drop off wifi entirely — a known Quick Share bug.
> The fix is counter-intuitive: **turn on the phone's Wi-Fi hotspot**, even though
> nothing will connect to it. That stops Quick Share tearing down the wifi connection.
> See [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

## How it works

```
Phone or desktop                      Kindle
────────────────                      ──────
 mDNS query  ──────────────────────▶  kindshare advertises
                                       _FC9F5ED42C8A._tcp
 TCP :12345  ──────────────────────▶  UKEY2 handshake (P-256 ECDH)
 encrypted frames ─────────────────▶  AES-256-CBC + HMAC-SHA256
 Introduction / PayloadTransfer ───▶  /mnt/us/documents
```

Three components with strict ownership:

1. **`kindshare -daemon`** owns the listener and the mDNS registration. It is *not* run
   from KOReader — KOReader isn't always open, and "receiving whenever wifi is up" can't
   depend on a reader app being in the foreground.
2. **`kindle-ap.sh`** does SoftAP only, and never touches the receiver.
3. **The KOReader plugin** is a control surface, not a service host.

The daemon polls the interface and **re-advertises whenever the address changes**. This
matters more than it sounds: mDNS publishes an A record for the address held at
registration time, and nothing crashes when your DHCP lease changes — so a plain
restart-on-crash supervisor would report everything healthy while the phone quietly fails
to connect. The same mechanism gets SoftAP, wifi toggles, and resume-from-sleep for free.

**The mDNS responder is ours** (`src/kindshare/mdns.go`) rather than a library, because
the obvious library choice answers queries for only three names and the host name your
own SRV record points at is not one of them. Android and Windows take the address out of
the additional records and never notice; a stricter resolver asks the address question
properly, gets silence, and ends up listing a device it cannot dial. That was the whole
of "works on my phone, not on my friend's Mac". Ours answers address queries,
re-announces every 45s so a dropped multicast reply is not fatal, sends with the IP TTL
255 that RFC 6762 asks for, and rebuilds its socket on address change and resume, since
multicast group membership dies quietly with the interface.

## Limitations

- **Receiving only.** Sending from the Kindle isn't implemented.
- **No transfers while asleep.** The Voyage suspends aggressively and wifi goes with it.
  The daemon re-registers on resume, but a sleeping Kindle can't receive.
- **Not paired.** We hold no Nearby Share certificates, so the phone shows the Kindle as
  an unknown device and `PairedKeyResult` is answered `UNABLE`. Transfers work; you just
  don't get the "known contact" treatment.
- **Transfers are auto-accepted.** There's no practical way to prompt mid-transfer on
  e-ink. Anything offered to this device is written to `/mnt/us/documents`.
- **SoftAP runs WPA2 by design**, not for secrecy — on an open BSS this firmware never
  authorizes the station and no data flows at all. See
  [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

## Credits

The protocol work rests on prior reverse engineering:

- [grishka/NearDrop](https://github.com/grishka/NearDrop) — the protocol documentation
  and the collected `.proto` files
- [Martichou/rquickshare](https://github.com/Martichou/rquickshare) — a Rust
  implementation, and the crucial detail that BLE is only needed for the *other*
  direction
- [google/ukey2](https://github.com/google/ukey2) — the handshake specification
- [NiLuJe](https://www.mobileread.com/forums/showthread.php?t=186645)'s usbnetwork hack
  and `fbink`

`proto/` contains definitions collected by NearDrop from Chromium sources, redistributed
here under their original BSD terms.

## License

MIT — see [LICENSE](LICENSE).
