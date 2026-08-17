# Porting

Read [COMPATIBILITY.md](COMPATIBILITY.md) first. The headline: **the Quick Share receiver
does not need access-point support**, so porting it is mostly a matter of paths and
startup integration. The SoftAP half is the part that depends on hardware.

## Porting the receiver (the easy, portable half)

### 1. Build

```sh
cd src/kindshare
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o kindshare .
```

Adjust `GOARCH`/`GOARM` for your target. `CGO_ENABLED=0` gives a fully static binary, so
the device's libc version is irrelevant — this is what makes it work on a 2014 e-reader.

To regenerate the protobuf bindings (only needed if you change `proto/`):

```sh
protoc --proto_path=proto --go_out=. --go_opt=module=kindshare \
  --go_opt=Msecuremessage.proto=kindshare/pb/securemessage \
  --go_opt=Msecuregcm.proto=kindshare/pb/securegcm \
  --go_opt=Mukey.proto=kindshare/pb/securegcm \
  --go_opt=Mdevice_to_device_messages.proto=kindshare/pb/securegcm \
  --go_opt=Moffline_wire_formats.proto=kindshare/pb/connections \
  --go_opt=Mwire_format.proto=kindshare/pb/sharing \
  --go_opt=Msharing_enums.proto=kindshare/pb/sharingenums \
  proto/*.proto
```

### 2. Adjust paths

Everything device-specific is at the top of each file:

| File | Change |
|---|---|
| `device/kindshare-svc.sh` | `BASE`, `DEST`, `PORT` |
| `device/kindle-ap.sh` | `BASE`, `AP_IP`, `AP_NET` |
| `plugin/kindleap.koplugin/main.lua` | `BASE`, `AP`, `SVC`, log paths |
| `device/kindshare.conf` | the whole file is upstart-specific |

On non-Kindle devices `/mnt/us` becomes e.g. `/mnt/onboard` (Kobo).

### 3. Firewall

Only if your device filters input. Check:

```sh
iptables -L INPUT -n | head -2      # look for "policy DROP"
```

If it says `DROP`, the daemon's `ensureFirewall()` handles it — it adds and re-asserts
rules for tcp/12345 and udp/5353. If `iptables` doesn't exist, it logs and carries on.

### 4. Start at boot

This is the only genuinely platform-specific part.

- **Kindle (upstart):** `device/kindshare.conf` → `/etc/upstart/`, keyed off
  `start on started volumd` because that's what makes `/mnt/us` available.
- **systemd:** a unit with `After=network.target`, `Restart=always`.
- **Anything else:** the daemon is happy to be launched by any supervisor. It stays in
  the foreground, logs to stdout, and never exits on its own.

Keep the **flag-file pattern** if your root filesystem is read-only: install the job once,
then have it check for a flag file on writable storage. It means toggling autostart never
needs a remount.

## Devices without AP mode

You lose only the no-network case. The receiver works unchanged over any network both
devices are on. Practical setups:

1. **Existing wifi** — nothing to do; this is the normal case.
2. **The phone's hotspot** — the reader joins it, the phone shares to it. Works fine, and
   is what SoftAP was built to replace.
3. **A pocket AP** — a Pi Zero W or travel router, if you want the offline case without
   AP support on the reader.

Only option 1 and 2 need zero extra hardware, and both are fully supported today.

## Building hostapd

Needed only for SoftAP. `hostapd` is not present on Kindle firmware. `build/Dockerfile`
cross-compiles a static armv7 binary under QEMU:

```sh
cd build
docker build --platform linux/arm/v7 -t kindle-hostapd:2.11 .
cid=$(docker create --platform linux/arm/v7 kindle-hostapd:2.11)
docker cp "$cid:/out/hostapd" .
docker rm "$cid"
```

Two things that will bite you if you build it yourself:

- **Don't pass `CFLAGS` on the make command line.** hostapd's Makefile does
  `CFLAGS += -I../src`, and a command-line assignment overrides that outright — every
  file then fails on `utils/includes.h`. Export them as environment variables instead.
- **libnl must live where hostapd looks.** For `CONFIG_LIBNL32` it hardcodes `-lnl-3` and
  `-I/usr/include/libnl3` and never consults `pkg-config`, so a custom `--prefix` is
  invisible. Build libnl with `--prefix=/usr`.

The shipped `device/hostapd.conf` is deliberately conservative — 802.11g, no HT, no WMM,
WPA2-PSK, `dtim_period=1`. Each of those is load-bearing on AR6003; see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) before "modernising" it.

## The design constraint worth keeping

**Control must live on the device.** Bringing up an AP necessarily destroys the network
path used to trigger it. Three separate remote approaches died on this: the SSH session
issuing the command, the ability to confirm success remotely, and finally the controlling
laptop's own uplink (one radio — joining the reader's AP killed the session driving it).

So: the UI runs on the device, and diagnostics are written to persistent storage rather
than streamed out, because the interesting failures happen exactly when nothing can be
reached. `kindle-ap.sh` also arms a **dead-man timer** that reverts to normal wifi if no
client connects, so a failed experiment costs a wait rather than a walk to the device.
