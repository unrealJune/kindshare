#!/bin/sh
#
# SoftAP control for the Kindle Voyage (AR6003 / ath6kl_sdio, nl80211).
#
# Bringing up the AP takes wlan0 away from the framework, which kills SSH-over-wifi.
# That is the only way back in unless usbnet is bound on the host, so `up` arms a
# DEAD-MAN TIMER: if nobody runs `commit` within $REVERT_AFTER seconds, the device
# tears the AP down and restarts wifid by itself. Losing the session then costs a
# wait, not a trip to the touchscreen.
#
#   kindle-ap.sh up       bring up AP + DHCP, arm the timer
#   kindle-ap.sh commit   disarm the timer (only once you have confirmed access)
#   kindle-ap.sh down     tear down and restore normal wifi
#   kindle-ap.sh status   what is running right now

set -u

BASE=/mnt/us/kindler
HOSTAPD=$BASE/bin/hostapd
CONF=$BASE/etc/hostapd.conf
DHCPCONF=$BASE/etc/udhcpd.conf
LOG=/tmp/kindle-ap.log

AP_IP=192.168.55.1
AP_NET=255.255.255.0
KEEP=/tmp/ap-keepalive
REVERT_AFTER=${REVERT_AFTER:-600}

log() { echo "[$(date '+%H:%M:%S')] $*" | tee -a "$LOG"; }

# Kill background helpers from any previous run. Without this a stale dead-man
# timer keeps counting and tears down a later, working session.
kill_helpers() {
    for pf in /tmp/ap-timer.pid /tmp/ap-sampler.pid; do
        if [ -f "$pf" ]; then
            p=$(cat "$pf" 2>/dev/null)
            if [ -n "$p" ]; then
                # The subshell first, then any sleep it is parked in.
                kill "$p" 2>/dev/null
                for c in $(ps -o pid= -o ppid= 2>/dev/null | awk -v pp="$p" '$2==pp {print $1}'); do
                    kill "$c" 2>/dev/null
                done
            fi
            rm -f "$pf"
        fi
    done
}

# The Kindle runs iptables with `policy DROP` on INPUT, so every port we expect
# to serve has to be opened explicitly or it simply times out.
open_ports() {
    # Blanket-accept on wlan0 while the AP is up. The INPUT policy is DROP and
    # DHCP DISCOVER arrives as a broadcast from 0.0.0.0, which is exactly the
    # kind of traffic a per-port rule is easy to miss. The AP is open and
    # short-lived anyway, so this removes the firewall as a variable entirely.
    iptables -C INPUT -i wlan0 -j ACCEPT 2>/dev/null || \
        iptables -I INPUT 1 -i wlan0 -j ACCEPT
    for p in 22 2222 8080 12345; do
        iptables -C INPUT -p tcp --dport "$p" -j ACCEPT 2>/dev/null || \
            iptables -I INPUT 1 -p tcp --dport "$p" -j ACCEPT
    done
    # mDNS discovery for Quick Share.
    iptables -C INPUT -p udp --dport 5353 -j ACCEPT 2>/dev/null || \
        iptables -I INPUT 1 -p udp --dport 5353 -j ACCEPT
    # udhcpd needs to hear DHCP discovers.
    iptables -C INPUT -p udp --dport 67 -j ACCEPT 2>/dev/null || \
        iptables -I INPUT 1 -p udp --dport 67 -j ACCEPT
    iptables -C OUTPUT -p udp --sport 67 -j ACCEPT 2>/dev/null || \
        iptables -I OUTPUT 1 -p udp --sport 67 -j ACCEPT
}

close_ports() {
    # Only remove what the AP itself owns. tcp/12345 and udp/5353 belong to the
    # always-on kindshare daemon, which keeps running in station mode - deleting
    # them here silently broke Quick Share every time the AP was torn down.
    iptables -D INPUT -i wlan0 -j ACCEPT 2>/dev/null
    iptables -D INPUT -p udp --dport 67 -j ACCEPT 2>/dev/null
    iptables -D OUTPUT -p udp --sport 67 -j ACCEPT 2>/dev/null
    iptables -D INPUT -p tcp --dport 8080 -j ACCEPT 2>/dev/null
}

ap_up() {
    rm -f "$KEEP"
    : > "$LOG"
    kill_helpers
    # Also kill receivers left over from manual testing: they hold :8080 and
    # :12345, and a second copy dies with "address already in use" - silently
    # leaving the AP with nothing listening on it.
    killall kindrop >/dev/null 2>&1
    log "killed any leftover timer/sampler/receiver from a previous run"
    log "stopping framework wifi management"
    stop wifid >/dev/null 2>&1
    killall wpa_supplicant >/dev/null 2>&1
    sleep 2

    # `stop wifid` can take the module (and wlan0) with it.
    if [ ! -d /sys/class/net/wlan0 ]; then
        log "wlan0 gone, reloading ath6kl_sdio"
        modprobe ath6kl_sdio
        sleep 3
    fi
    if [ ! -d /sys/class/net/wlan0 ]; then
        log "FATAL: no wlan0 after modprobe"
        ap_down
        return 1
    fi

    # Dead-man timer. It counts IDLE time, not wall-clock: as long as a station
    # is associated the countdown resets, so a live transfer is never guillotined
    # mid-file. It only fires when nothing has connected for $REVERT_AFTER, which
    # is exactly the "the AP is broken and I have lost my way in" case.
    # Generation token. Timers and samplers from earlier runs keep running - a
    # stale 600s timer armed by a previous `up` tore down a later, working
    # session mid-use. Each run stamps a token; background helpers exit as soon
    # as it no longer matches theirs.
    RUNID="$(date +%s)-$$"
    echo "$RUNID" > /tmp/ap-runid

    log "arming dead-man timer (${REVERT_AFTER}s idle, run $RUNID)"
    ( idle=0; prev=0
      while [ "$idle" -lt "$REVERT_AFTER" ]; do
          sleep 15
          [ "$(cat /tmp/ap-runid 2>/dev/null)" = "$RUNID" ] || exit 0
          [ -f "$KEEP" ] && { log "commit seen - timer disarmed"; exit 0; }
          # NOT `iw station dump`: this driver does not implement dump_station in
          # AP mode, so it reports zero even with a client associated, and the
          # timer would fire during an active transfer. hostapd's own events are
          # the only reliable source.
          c=$(grep -ac "AP-STA-CONNECTED" /tmp/hostapd.log 2>/dev/null || echo 0)
          d=$(grep -ac "AP-STA-DISCONNECTED" /tmp/hostapd.log 2>/dev/null || echo 0)
          if [ "${c:-0}" -gt "${prev:-0}" ] || [ "${c:-0}" -gt "${d:-0}" ]; then
              idle=0; prev=${c:-0}
          else
              idle=$((idle + 15))
          fi
      done
      log "dead-man expired with no client and no commit - reverting"
      ap_down ) >/dev/null 2>&1 &
    echo $! > /tmp/ap-timer.pid

    open_ports

    # -dd, to its own file: we need to see the per-station events. If the driver
    # refuses NL80211_CMD_SET_STATION(AUTHORIZED), stations will associate and
    # then silently carry no data, which is what we are chasing.
    # NOT -B -f: hostapd's -f (log to file) is compiled in only with
    # CONFIG_DEBUG_FILE=y, which this build lacks, so -f silently produced
    # nothing. Run it in the foreground under nohup and redirect instead - same
    # result, no rebuild.
    log "starting hostapd (debug -> /tmp/hostapd.log)"
    ifconfig wlan0 up
    : > /tmp/hostapd.log
    nohup "$HOSTAPD" -dd "$CONF" >> /tmp/hostapd.log 2>&1 &
    echo $! > /tmp/hostapd.pid
    sleep 3
    if ! kill -0 "$(cat /tmp/hostapd.pid)" 2>/dev/null; then
        log "hostapd died on startup - last lines:"
        tail -20 /tmp/hostapd.log | tee -a "$LOG"
        ap_down
        return 1
    fi

    log "addressing wlan0 as $AP_IP"
    ifconfig wlan0 "$AP_IP" netmask "$AP_NET" up

    # -f (foreground) + redirect, so we can see whether DISCOVERs actually
    # arrive. Daemonised udhcpd logs to syslog, which we cannot read here. If
    # this file stays empty while a client is associated, the 802.11 data path
    # is dead and DHCP is merely the messenger.
    log "starting udhcpd (log -> /tmp/udhcpd.log)"
    : > /tmp/udhcpd.leases
    : > /tmp/udhcpd.log
    nohup udhcpd -f "$DHCPCONF" >> /tmp/udhcpd.log 2>&1 &
    echo $! > /tmp/udhcpd.pid

    # Without this there is nothing to talk to on the new network but the kernel.
    log "starting kindrop receiver on :8080"
    mkdir -p /mnt/us/documents
    nohup "$BASE/bin/kindrop" -addr :8080 -dest /mnt/us/documents \
        > /tmp/kindrop.log 2>&1 &

    # Quick Share discovery probe (stage 1). Advertises the Nearby Share mDNS
    # service so we can find out whether the phone will discover us over the
    # SoftAP at all - the one assumption the whole feature rests on.
    # kindshare is NOT started here. The always-on daemon owns the Quick Share
    # listener and re-registers itself whenever wlan0's address changes - which
    # is exactly what happens when this AP comes up (wlan0 becomes 192.168.55.1).
    # Starting a second copy here would only collide on the port.
    if ps | grep -q "[k]indshare"; then
        log "kindshare daemon is running; it will re-advertise on $AP_IP"
    else
        log "WARNING: kindshare daemon is not running - Quick Share will not work"
    fi

    # Rolling timeline. Every interesting failure happens precisely while nobody
    # can look, so sample state every 10s to a file we can read afterwards.
    # `station dump` carries the authorized/associated flags, which is the direct
    # test of "client associates but the data path never opens".
    : > /tmp/ap-samples.log
    ( while [ -f /tmp/hostapd.pid ] && [ "$(cat /tmp/ap-runid 2>/dev/null)" = "$RUNID" ]; do
        {
            echo "--- $(date '+%H:%M:%S') ---"
            # station dump is a dead end on this driver; hostapd's counters are
            # the real association state.
            echo "hostapd connects=$(grep -ac 'AP-STA-CONNECTED' /tmp/hostapd.log 2>/dev/null) disconnects=$(grep -ac 'AP-STA-DISCONNECTED' /tmp/hostapd.log 2>/dev/null) authorized=$(grep -ac 'authorized=1' /tmp/hostapd.log 2>/dev/null)"
            echo "eapol: $(grep -ac 'EAPOL' /tmp/hostapd.log 2>/dev/null) / 4way: $(grep -ac 'WPA: ' /tmp/hostapd.log 2>/dev/null)"
            echo "dhcp: $(grep -ac 'OFFER\|ACK' /tmp/udhcpd.log 2>/dev/null) events"
            echo "leases: $(wc -c < /tmp/udhcpd.leases 2>/dev/null) bytes"
            echo "rx/tx on wlan0:"; grep -a wlan0 /proc/net/dev
            iptables -L INPUT -n -v 2>/dev/null | grep -aE 'wlan0|dpt:67'
        } >> /tmp/ap-samples.log 2>&1
        # 3s, not 10s: associations have been lasting only a few seconds, and a
        # 10s cadence missed every one of them.
        sleep 3
    done ) >/dev/null 2>&1 &
    echo $! > /tmp/ap-sampler.pid

    # Record the state we could not see remotely last time. The failure mode we
    # are chasing - client associates but never gets a lease - looks identical
    # from outside whether wlan0 lost its address, udhcpd died, or the firewall
    # ate the DISCOVER. These three lines tell them apart.
    sleep 2
    {
        echo "--- post-start verification ---"
        echo "[wlan0]";   ifconfig wlan0 | head -3
        echo "[iw]";      iw dev wlan0 info
        echo "[procs]";   ps | grep -E "[h]ostapd|[u]dhcpd|[k]indrop"
        echo "[udp/67]";  iptables -L INPUT -n -v | grep -E "dpt:67|policy"
        echo "[leases]";  ls -l /tmp/udhcpd.leases 2>&1
    } >> "$LOG" 2>&1

    log "AP up. Run 'kindle-ap.sh commit' within ${REVERT_AFTER}s to keep it."
    ap_status
}

ap_down() {
    log "tearing down AP"
    kill_helpers
    [ -f /tmp/hostapd.pid ] && kill "$(cat /tmp/hostapd.pid)" 2>/dev/null
    [ -f /tmp/udhcpd.pid ] && kill "$(cat /tmp/udhcpd.pid)" 2>/dev/null
    killall hostapd  >/dev/null 2>&1
    killall udhcpd   >/dev/null 2>&1
    killall kindrop  >/dev/null 2>&1
    rm -f /tmp/hostapd.pid /tmp/udhcpd.pid /tmp/ap-runid
    ifconfig wlan0 0.0.0.0 down 2>/dev/null
    close_ports
    log "restarting wifid"
    start wifid >/dev/null 2>&1
    log "reverted"
}

ap_status() {
    echo "--- interface ---"
    iw dev 2>/dev/null | grep -E "Interface|type|channel"
    ifconfig wlan0 2>/dev/null | head -2
    echo "--- processes ---"
    ps | grep -E "[h]ostapd|[u]dhcpd|[w]ifid" || echo "(none)"
    echo "--- stations ---"
    iw dev wlan0 station dump 2>/dev/null | grep -c Station
    echo "--- keepalive ---"
    [ -f "$KEEP" ] && echo "committed" || echo "dead-man ARMED"
}

case "${1:-}" in
    up)     ap_up ;;
    down)   ap_down ;;
    commit) touch "$KEEP"; echo "dead-man disarmed - AP will persist" ;;
    status) ap_status ;;
    *)      echo "usage: $0 {up|commit|down|status}"; exit 1 ;;
esac
