#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SIDE=${1:-}
if [[ "$SIDE" != iran && "$SIDE" != foreign ]]; then
    echo "Usage: bash install.sh [status|start|stop|restart|enable|disable|list]" >&2
    exit 2
fi
shift

BASE="splitd-v3-$SIDE"
CONFDIR=/etc/splitd-v3
BINDIR=/usr/local/bin
UNITDIR=/etc/systemd/system
TEMPLATE="$ROOT/$SIDE/splitd.conf"

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this script as root." >&2
    exit 1
fi

list_instances() {
    local f name
    for f in "$UNITDIR/$BASE.service" "$UNITDIR/$BASE"-*.service; do
        [ -f "$f" ] || continue
        name=$(basename "$f" .service)
        case "$name" in "$BASE"|"$BASE"-*) echo "$name" ;; esac
    done
}

conf_for() {
    if [ "$1" = "$BASE" ]; then
        echo "$CONFDIR/$SIDE.conf"
    else
        echo "$CONFDIR/$SIDE-${1#"$BASE"-}.conf"
    fi
}

bin_for() {
    if [ "$1" = "$BASE" ]; then
        echo "$BINDIR/splitd-v3"
    else
        echo "$BINDIR/splitd-v3-$SIDE-${1#"$BASE"-}"
    fi
}

getconf() {
    [ -f "$2" ] || return 0
    sed -n "s/^$1=//p" "$2" 2>/dev/null | tail -1
}

valid_port() {
    [[ "${1:-}" =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

next_instance() {
    local n=1 name
    while :; do
        if [ "$n" = 1 ]; then name="$BASE"; else name="$BASE-$n"; fi
        [ -f "$UNITDIR/$name.service" ] || { echo "$name"; return; }
        n=$((n + 1))
    done
}

instance_by_index() {
    local wanted="$1" i=0 inst
    for inst in $(list_instances); do
        i=$((i + 1))
        if [ "$i" = "$wanted" ]; then echo "$inst"; return 0; fi
    done
    return 1
}

resolve_instance() {
    local wanted="${1:-$BASE}" found
    if [ -f "$UNITDIR/$wanted.service" ]; then echo "$wanted"; return 0; fi
    if [[ "$wanted" =~ ^[0-9]+$ ]]; then
        found=$(instance_by_index "$wanted") || return 1
        echo "$found"
        return 0
    fi
    return 1
}

port_busy() {
    ss -H -lnt 2>/dev/null | awk '{print $4}' | grep -qE ":$1$"
}

port_used_elsewhere() {
    local port="$1" skip="$2" host="${3:-}" inst conf value local_port
    for inst in $(list_instances); do
        [ "$inst" = "$skip" ] && continue
        conf=$(conf_for "$inst")
        if [ "$SIDE" = iran ]; then
            value=$(getconf LOCAL_ADDR "$conf"); local_port=${value##*:}
            if [ "$(getconf DL_PORT "$conf")" = "$port" ] ||
               [ "$(getconf UL_PORT "$conf")" = "$port" ] ||
               [ "$local_port" = "$port" ]; then
                return 0
            fi
        elif [ "$(getconf IRAN_HOST "$conf")" = "$host" ]; then
            if [ "$(getconf DL_PORT "$conf")" = "$port" ] ||
               [ "$(getconf UL_PORT "$conf")" = "$port" ]; then
                return 0
            fi
        fi
    done
    return 1
}

find_free_port() {
    local candidate="$1" skip="$2" host="${3:-}"
    while [ "$candidate" -le 65535 ]; do
        if ! port_used_elsewhere "$candidate" "$skip" "$host" &&
           { [ "$SIDE" = foreign ] || ! port_busy "$candidate"; }; then
            echo "$candidate"
            return 0
        fi
        candidate=$((candidate + 1))
    done
    return 1
}

show_instances() {
    local inst conf state a b c
    for inst in $(list_instances); do
        conf=$(conf_for "$inst")
        state=$(systemctl is-active "$inst" 2>/dev/null || true)
        if [ "$SIDE" = iran ]; then
            a=$(getconf LOCAL_ADDR "$conf"); b=$(getconf DL_PORT "$conf"); c=$(getconf UL_PORT "$conf")
            printf '   %-22s panel=%-16s dl=%-6s ul=%-6s [%s]\n' "$inst" "${a:--}" "${b:--}" "${c:--}" "${state:-unknown}"
        else
            a=$(getconf IRAN_HOST "$conf"); b=$(getconf DL_PORT "$conf"); c=$(getconf UL_PORT "$conf")
            printf '   %-22s iran=%-16s dl=%-6s ul=%-6s [%s]\n' "$inst" "${a:--}" "${b:--}" "${c:--}" "${state:-unknown}"
        fi
    done
}

manage_action() {
    local action="$1" name
    name=$(resolve_instance "${2:-$BASE}") || {
        echo "No V3 instance matches '${2:-$BASE}'. Run 'bash install.sh list' to see instances." >&2
        exit 1
    }
    case "$action" in
        status)  exec systemctl status --no-pager "$name" ;;
        start)   systemctl start "$name" ;;
        stop)    systemctl stop "$name" ;;
        restart) systemctl restart "$name" ;;
        enable)  systemctl enable "$name" ;;
        disable) systemctl disable "$name" ;;
    esac
    systemctl is-active "$name" || true
}

ACTION=${1:-menu}
case "$ACTION" in
    status|start|stop|restart|enable|disable)
        manage_action "$ACTION" "${2:-$BASE}"
        exit 0
        ;;
    list)
        if [ -z "$(list_instances)" ]; then echo "No V3 instances installed."; else show_instances; fi
        exit 0
        ;;
    help|--help|-h)
        echo "Usage: bash install.sh [status|start|stop|restart|enable|disable|list] [instance-number-or-name]"
        echo "With no argument, opens the interactive V3 instance manager."
        echo "Running the manager installs or reconfigures the selected V3 instance; fixed-interval restarts are not used."
        exit 0
        ;;
    menu|install|upgrade) ;;
    *)
        echo "Unknown command: $ACTION" >&2
        echo "Usage: bash install.sh [status|start|stop|restart|enable|disable|list]" >&2
        exit 2
        ;;
esac

G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[31m'; B=$'\033[1m'; N=$'\033[0m'
echo
printf '%s+--------------------------------------------------+%s\n' "$B" "$N"
printf '%s|   splitd V3 -- instance manager (%s side)       |%s\n' "$B" "$SIDE" "$N"
printf '%s+--------------------------------------------------+%s\n\n' "$B" "$N"

INSTANCES=$(list_instances)
TARGET_NAME=
NEW=0
if [ -n "$INSTANCES" ]; then
    echo "${Y}installed V3 instances:${N}"
    show_instances
    echo
    echo "   ${B}n${N} = add a parallel V3 instance"
    echo "   ${B}1,2,3...${N} = reconfigure that instance"
    echo "   ${B}d${N} = DELETE an instance (stop it and remove its files)"
    echo
    printf '%s' "choice [n]: "
    read -r choice
    case "$choice" in
        ""|[nN]) TARGET_NAME=$(next_instance); NEW=1 ;;
        [dD])
            printf '%s' "which instance to delete? [1,2,3...] "
            read -r choice
            DEL=$(instance_by_index "$choice") || { echo "No such instance." >&2; exit 1; }
            CONF=$(conf_for "$DEL"); BIN=$(bin_for "$DEL"); UNIT="$UNITDIR/$DEL.service"
            echo
            echo "${Y}about to DELETE:${N} ${B}$DEL${N}"
            echo "   stop + disable : systemctl disable --now $DEL"
            echo "   remove unit    : $UNIT"
            echo "   remove config  : $CONF"
            echo "   remove binary  : $BIN"
            printf '%s' "sure? [y/N] "
            read -r confirm
            case "$confirm" in [yY]*) ;; *) echo "cancelled."; exit 0 ;; esac
            systemctl disable --now "$DEL" >/dev/null 2>&1 || true
            rm -f -- "$UNIT" "$CONF" "$BIN"
            rm -f -- "$UNITDIR/$DEL.service.d/zz-splitd-v3-logging.conf"
            rmdir "$UNITDIR/$DEL.service.d" 2>/dev/null || true
            systemctl daemon-reload
            echo "${G}removed $DEL${N}"
            echo
            show_instances
            exit 0
            ;;
        *)
            TARGET_NAME=$(instance_by_index "$choice") || { echo "No such instance." >&2; exit 1; }
            ;;
    esac
else
    TARGET_NAME=$(next_instance)
    NEW=1
fi

CONF=$(conf_for "$TARGET_NAME")
BIN=$(bin_for "$TARGET_NAME")
UNIT="$UNITDIR/$TARGET_NAME.service"
if [ "$NEW" = 1 ]; then echo "${B}creating new V3 instance:${N} ${G}$TARGET_NAME${N}"; else echo "${B}reconfiguring V3 instance:${N} ${G}$TARGET_NAME${N}"; fi
echo "   unit   : $UNIT"
echo "   config : $CONF"
echo "   binary : $BIN"
echo

if [ -f "$CONF" ]; then
    D_LOCAL=$(getconf LOCAL_ADDR "$CONF")
    D_DL=$(getconf DL_PORT "$CONF")
    D_UL=$(getconf UL_PORT "$CONF")
    D_IRAN=$(getconf IRAN_HOST "$CONF")
    D_TARGET=$(getconf TARGET "$CONF")
    D_FLOW=$(getconf FLOW_STALE_SEC "$CONF")
    D_LOG=$(getconf LOG "$CONF")
else
    D_LOCAL=$(getconf LOCAL_ADDR "$TEMPLATE")
    D_DL=$(getconf DL_PORT "$TEMPLATE")
    D_UL=$(getconf UL_PORT "$TEMPLATE")
    D_IRAN=$(getconf IRAN_HOST "$TEMPLATE")
    D_TARGET=$(getconf TARGET "$TEMPLATE")
    D_FLOW=$(getconf FLOW_STALE_SEC "$TEMPLATE")
    D_LOG=$(getconf LOG "$TEMPLATE")
fi
[ -n "$D_FLOW" ] || D_FLOW=15
[ -n "$D_LOG" ] || D_LOG=off

if [ "$NEW" = 1 ]; then
    if [ "$SIDE" = iran ]; then
        [ -n "$D_LOCAL" ] || D_LOCAL=127.0.0.1:92
        PANEL_PORT=${D_LOCAL##*:}
        if port_busy "$PANEL_PORT" || port_used_elsewhere "$PANEL_PORT" "$TARGET_NAME"; then
            PANEL_PORT=$(find_free_port $((PANEL_PORT + 1)) "$TARGET_NAME") || { echo "No free panel port." >&2; exit 1; }
            D_LOCAL="127.0.0.1:$PANEL_PORT"
        fi
        [ -n "$D_DL" ] || D_DL=12797
        D_DL=$(find_free_port "$D_DL" "$TARGET_NAME") || { echo "No free DL port." >&2; exit 1; }
        [ -n "$D_UL" ] || D_UL=$((D_DL + 1))
        if [ "$D_UL" = "$D_DL" ] || port_busy "$D_UL" || port_used_elsewhere "$D_UL" "$TARGET_NAME"; then
            D_UL=$(find_free_port $((D_DL + 1)) "$TARGET_NAME") || { echo "No free UL port." >&2; exit 1; }
        fi
    else
        if [ -z "$D_IRAN" ]; then
            first=$(list_instances | head -1)
            [ -z "$first" ] || D_IRAN=$(getconf IRAN_HOST "$(conf_for "$first")")
        fi
        [ -n "$D_DL" ] || D_DL=12797
        [ -n "$D_UL" ] || D_UL=$((D_DL + 1))
        if [ -n "$D_IRAN" ]; then
            D_DL=$(find_free_port "$D_DL" "$TARGET_NAME" "$D_IRAN") || { echo "No free DL port for this Iran host." >&2; exit 1; }
            if [ "$D_UL" = "$D_DL" ] || port_used_elsewhere "$D_UL" "$TARGET_NAME" "$D_IRAN"; then
                D_UL=$(find_free_port $((D_DL + 1)) "$TARGET_NAME" "$D_IRAN") || { echo "No free UL port for this Iran host." >&2; exit 1; }
            fi
        fi
        [ -n "$D_TARGET" ] || D_TARGET=127.0.0.1:443
    fi
fi

SKIP_ASK=
if [ "$NEW" = 0 ]; then
    printf '%s' "change the settings? [y/N] "
    read -r answer
    case "$answer" in [yY]*) ;; *) SKIP_ASK=1 ;; esac
    echo
fi

if [ -z "$SKIP_ASK" ]; then
    echo "${B}settings (Enter = keep the suggested value)${N}"
    if [ "$SIDE" = iran ]; then
        while :; do
            printf '  panel address (loopback) [%s]: ' "$D_LOCAL"
            read -r value; LOCAL_ADDR=${value:-$D_LOCAL}
            if [[ "$LOCAL_ADDR" =~ ^127\.0\.0\.1:[0-9]{1,5}$ ]] && valid_port "${LOCAL_ADDR##*:}"; then break; fi
            echo "    enter a loopback address such as 127.0.0.1:92"
        done
    else
        while :; do
            printf '  Iran server IP/host [%s]: ' "${D_IRAN:-none}"
            read -r value; IRAN_HOST=${value:-$D_IRAN}
            if [[ "$IRAN_HOST" =~ ^[A-Za-z0-9._-]+$ ]]; then break; fi
            echo "    enter a valid host name or IP address"
        done
    fi
    while :; do
        printf '  download flow port [%s]: ' "$D_DL"
        read -r value; DL_PORT=${value:-$D_DL}
        if valid_port "$DL_PORT"; then break; fi
        echo "    give a port number between 1 and 65535"
    done
    while :; do
        printf '  upload flow port [%s]: ' "$D_UL"
        read -r value; UL_PORT=${value:-$D_UL}
        if valid_port "$UL_PORT" && [ "$UL_PORT" != "$DL_PORT" ]; then break; fi
        echo "    choose a different port number between 1 and 65535"
    done
    if [ "$SIDE" = foreign ]; then
        while :; do
            printf '  local target on this box [%s]: ' "$D_TARGET"
            read -r value; TARGET=${value:-$D_TARGET}
            if [[ "$TARGET" =~ ^[A-Za-z0-9._-]+:[0-9]{1,5}$ ]] && valid_port "${TARGET##*:}"; then break; fi
            echo "    use host:port format, for example 127.0.0.1:443"
        done
    fi
    while :; do
        printf '  stale-flow recovery threshold in seconds [%s]: ' "$D_FLOW"
        read -r value; FLOW_STALE_SEC=${value:-$D_FLOW}
        if [[ "$FLOW_STALE_SEC" =~ ^[0-9]{1,4}$ ]] && (( 10#$FLOW_STALE_SEC >= 5 && 10#$FLOW_STALE_SEC <= 3600 )); then break; fi
        echo "    give a whole number from 5 to 3600"
    done
    printf '  write logs to the journal? [y/N]: '
    read -r value
    case "$value" in [yY]*) LOG=on ;; *) LOG=off ;; esac
else
    LOCAL_ADDR=$D_LOCAL
    DL_PORT=$D_DL
    UL_PORT=$D_UL
    IRAN_HOST=$D_IRAN
    TARGET=$D_TARGET
    FLOW_STALE_SEC=$D_FLOW
    LOG=$D_LOG
fi

if [ "$SIDE" = foreign ] && [ -z "${IRAN_HOST:-}" ]; then
    echo "Iran host is required." >&2
    exit 1
fi
if ! valid_port "$DL_PORT" || ! valid_port "$UL_PORT" || [ "$DL_PORT" = "$UL_PORT" ]; then
    echo "DL and UL must be different ports from 1 to 65535." >&2
    exit 1
fi
if [ "$SIDE" = iran ]; then
    PANEL_PORT=${LOCAL_ADDR##*:}
    if [ "$PANEL_PORT" = "$DL_PORT" ] || [ "$PANEL_PORT" = "$UL_PORT" ]; then
        echo "Panel, DL, and UL ports must be different." >&2
        exit 1
    fi
    for p in "$DL_PORT" "$UL_PORT" "$PANEL_PORT"; do
        if port_used_elsewhere "$p" "$TARGET_NAME"; then
            echo "Port $p is already assigned to another V3 instance." >&2
            exit 1
        fi
        if [ "$NEW" = 1 ] && port_busy "$p"; then
            echo "Port $p is already listening on this server." >&2
            exit 1
        fi
    done
else
    for p in "$DL_PORT" "$UL_PORT"; do
        if port_used_elsewhere "$p" "$TARGET_NAME" "$IRAN_HOST"; then
            echo "Port $p is already assigned to another V3 instance for $IRAN_HOST." >&2
            exit 1
        fi
    done
fi
case "$LOG" in on|off) ;; *) LOG=off ;; esac

build_binary() {
    local output="$1" bundled="$ROOT/$SIDE/splitd" build_message=
    if command -v go >/dev/null 2>&1; then
        if build_message=$(cd "$ROOT" && GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -o "$output" . 2>&1); then
            echo "   built V3 from source with the installed Go toolchain"
            return 0
        else
            echo "   local Go build unavailable; checking bundled binary"
            [ -z "$build_message" ] || printf '   %s\n' "$build_message"
        fi
    fi
    if [ -s "$bundled" ]; then
        cp "$bundled" "$output"
        echo "   using bundled V3 Linux binary"
        return 0
    fi
    echo "No usable Go toolchain or bundled V3 binary found." >&2
    return 1
}

echo
echo "${B}fixed-interval restart: disabled${N}"
echo "${B}flow recovery: replaces stalled one-way TCP lanes only${N}"
echo "${B}application logs: $LOG${N}"
echo

TMP=$(mktemp)
trap 'rm -f "$TMP"' EXIT
build_binary "$TMP"
install -d -m 755 "$BINDIR" "$CONFDIR" "$UNITDIR"
install -m 755 "$TMP" "$BIN"

if [ "$SIDE" = iran ]; then
    cat > "$CONF" <<EOF
# splitd V3 settings -- Iran side
LOCAL_ADDR=$LOCAL_ADDR
DL_PORT=$DL_PORT
UL_PORT=$UL_PORT
FLOW_STALE_SEC=$FLOW_STALE_SEC
LOG=$LOG
EOF
else
    cat > "$CONF" <<EOF
# splitd V3 settings -- Foreign side
IRAN_HOST=$IRAN_HOST
DL_PORT=$DL_PORT
UL_PORT=$UL_PORT
TARGET=$TARGET
FLOW_STALE_SEC=$FLOW_STALE_SEC
LOG=$LOG
EOF
fi
chmod 600 "$CONF"

LOGLINES=
if [ "$LOG" = off ]; then LOGLINES=$'StandardOutput=null\nStandardError=null'; fi
if [ "$SIDE" = iran ]; then
    cat > "$UNIT" <<EOF
[Unit]
Description=splitd V3 tunnel (Iran side)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
$LOGLINES
EnvironmentFile=$CONF
ExecStart=$BIN -mode iran -local \${LOCAL_ADDR} -dl 0.0.0.0:\${DL_PORT} -ul 0.0.0.0:\${UL_PORT} -flow-stale \${FLOW_STALE_SEC}
Restart=always
RestartSec=2
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
else
    cat > "$UNIT" <<EOF
[Unit]
Description=splitd V3 tunnel (Foreign side)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
$LOGLINES
EnvironmentFile=$CONF
ExecStart=$BIN -mode foreign -iran \${IRAN_HOST} -dlport \${DL_PORT} -ulport \${UL_PORT} -target \${TARGET} -flow-stale \${FLOW_STALE_SEC}
Restart=always
RestartSec=2
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF
fi

# The later-named drop-in also overrides an older quiet-mode override.conf,
# so the interactive log choice takes effect consistently on existing units.
LOG_DROPIN_DIR="$UNITDIR/$TARGET_NAME.service.d"
LOG_DROPIN="$LOG_DROPIN_DIR/zz-splitd-v3-logging.conf"
install -d -m 755 "$LOG_DROPIN_DIR"
if [ "$LOG" = off ]; then
    cat > "$LOG_DROPIN" <<'EOF'
[Service]
StandardOutput=null
StandardError=null
EOF
else
    cat > "$LOG_DROPIN" <<'EOF'
[Service]
StandardOutput=journal
StandardError=journal
EOF
fi
chmod 644 "$LOG_DROPIN"

if systemctl is-active --quiet "$TARGET_NAME" 2>/dev/null; then
    echo "-- stopping $TARGET_NAME --"
    systemctl stop "$TARGET_NAME"
fi
systemctl daemon-reload
systemctl enable "$TARGET_NAME" >/dev/null
systemctl restart "$TARGET_NAME"
sleep 2
if ! systemctl is-active --quiet "$TARGET_NAME"; then
    echo "${R}ERROR: $TARGET_NAME did not start.${N}" >&2
    echo "Check service status with: bash install.sh status" >&2
    exit 1
fi
echo "${G}OK: $TARGET_NAME is active.${N}"
if [ "$SIDE" = iran ]; then
    echo "Panel outbound: $LOCAL_ADDR"
    echo "Iran flow listeners: TCP $DL_PORT and TCP $UL_PORT"
else
    echo "Iran endpoint: $IRAN_HOST, TCP ports $DL_PORT and $UL_PORT"
    echo "Target: $TARGET"
fi
echo "Stale-flow threshold: ${FLOW_STALE_SEC}s; no periodic restart."
echo
echo "${B}all V3 instances${N}"
show_instances
echo
echo "Manage: bash install.sh status|start|stop|restart [instance-number-or-name]"
