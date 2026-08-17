#!/bin/sh
#
# Service control for the kindshare Quick Share receiver.
#
#   kindshare-svc.sh start      run the daemon now
#   kindshare-svc.sh stop       stop it now
#   kindshare-svc.sh restart    stop, then start
#   kindshare-svc.sh status     what is it doing
#   kindshare-svc.sh enable     start automatically at boot
#   kindshare-svc.sh disable    do not start at boot
#   kindshare-svc.sh log        recent log output
#
# Running now and starting at boot are deliberately separate: you can stop the
# service for this session without losing the boot setting, and vice versa.

set -u

BASE=/mnt/us/kindler
BIN=$BASE/bin/kindshare
ENABLE=$BASE/autostart
PIDFILE=/tmp/kindshare.pid
LOG=/tmp/kindshare.log
STATUS=/tmp/kindshare-status.json
DEST=/mnt/us/documents
PORT=12345

running() {
    pidof kindshare >/dev/null 2>&1
}

open_ports() {
    # INPUT policy is DROP on this device: without these the phone can see the
    # advertisement but its connection is silently dropped.
    iptables -C INPUT -p tcp --dport $PORT -j ACCEPT 2>/dev/null || \
        iptables -I INPUT 1 -p tcp --dport $PORT -j ACCEPT
    iptables -C INPUT -p udp --dport 5353 -j ACCEPT 2>/dev/null || \
        iptables -I INPUT 1 -p udp --dport 5353 -j ACCEPT
}

svc_start() {
    if running; then
        echo "already running (pid $(pidof kindshare))"
        return 0
    fi
    [ -x "$BIN" ] || { echo "missing or not executable: $BIN"; return 1; }
    open_ports
    mkdir -p "$DEST"
    nohup "$BIN" -daemon -name "Kindle Voyage" -iface wlan0 \
        -port $PORT -dest "$DEST" > "$LOG" 2>&1 &
    echo $! > "$PIDFILE"
    sleep 2
    if running; then
        echo "started (pid $(pidof kindshare))"
    else
        echo "FAILED to start - last lines:"
        tail -10 "$LOG" 2>/dev/null
        return 1
    fi
}

svc_stop() {
    if ! running; then
        echo "not running"
        rm -f "$PIDFILE"
        return 0
    fi
    killall kindshare 2>/dev/null
    sleep 2
    if running; then
        killall -9 kindshare 2>/dev/null
        sleep 1
    fi
    rm -f "$PIDFILE" "$STATUS"
    running && echo "still running - could not stop" || echo "stopped"
}

svc_status() {
    if running; then
        echo "service: RUNNING (pid $(pidof kindshare))"
    else
        echo "service: stopped"
    fi
    [ -f "$ENABLE" ] && echo "at boot : enabled" || echo "at boot : disabled"
    echo "wlan0   : $(ifconfig wlan0 2>/dev/null | sed -n 's/.*inet addr:\([0-9.]*\).*/\1/p')"
    echo "port    : $PORT $(iptables -L INPUT -n 2>/dev/null | grep -q "dpt:$PORT" && echo '(open)' || echo '(BLOCKED - no firewall rule)')"
    if [ -f "$STATUS" ]; then
        echo "--- daemon status ---"
        cat "$STATUS"
    fi
}

case "${1:-}" in
    start)   svc_start ;;
    stop)    svc_stop ;;
    restart) svc_stop; svc_start ;;
    status)  svc_status ;;
    enable)  touch "$ENABLE"; sync; echo "will start at boot" ;;
    disable) rm -f "$ENABLE"; sync; echo "will NOT start at boot" ;;
    log)     tail -40 "$LOG" 2>/dev/null || echo "(no log yet)" ;;
    *)       echo "usage: $0 {start|stop|restart|status|enable|disable|log}"; exit 1 ;;
esac
