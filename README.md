# kindshare

Receive files on a jailbroken Kindle straight from an Android phone's **Quick Share**
(Nearby Share) — no cable, no router required, no cloud, nothing to install on the phone.

It also turns the Kindle into a **Wi-Fi access point** for when there's no network to
share, so the phone can connect directly to the Kindle.

Files land in `/mnt/us/documents`, so an epub sent this way appears in your library.

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

## Use it

Open KOReader → menu → **Network** → **Quick Share / SoftAP**:

- **Receiving: ON / off** — start or stop the receiver
- **Start receiving at boot** — the autostart flag
- **Service status** — mode, address, whether the port is actually open, files received
- **Start access point** — for when there's no shared network
- **Diagnostics** / **Save diagnostics** — everything needed to debug a failure offline

Then on the phone: share a file, pick **Kindle Voyage**, done.

With **SoftAP**, join the Kindle's network first (default SSID `iceeibe`, WPA2). Quick
Share keeps working — the daemon follows the address change automatically.

## How it works

```
Android phone                         Kindle
─────────────                         ──────
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

The daemon polls the interface and **re-registers whenever the address changes**. This
matters more than it sounds: mDNS publishes an A record for the address held at
registration time, and nothing crashes when your DHCP lease changes — so a plain
restart-on-crash supervisor would report everything healthy while the phone quietly fails
to connect. The same mechanism gets SoftAP, wifi toggles, and resume-from-sleep for free.

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
