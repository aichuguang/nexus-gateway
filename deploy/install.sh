#!/usr/bin/env bash
# First-install only. This script never changes firewall, EasyTier, DNS or certd.
set -Eeuo pipefail
umask 077

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
INSTALL_DIR=/opt/gateway
DATA_DIR=/var/lib/gateway
TRAFFIC_LOGROTATE=/etc/logrotate.d/gateway-traffic
DEFAULT_SERVICE=nginx.service
CREATED_NGINX_MASK=0
STARTED=0
GATEWAY_IP="${GATEWAY_IP:-}"
MANAGEMENT_LISTEN="${MANAGEMENT_LISTEN:-$GATEWAY_IP:18080}"

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*"; }

valid_vpn_ipv4() {
    local a b c d octet
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS=. read -r a b c d <<< "$1"
    for octet in "$a" "$b" "$c" "$d"; do
        [[ "$octet" =~ ^(0|[1-9][0-9]{0,2})$ ]] || return 1
        (( octet <= 255 )) || return 1
    done
    # Match the application's IPv4 policy without guessing a VPN prefix.
    # A final octet of 0 or 255 can be a host address outside a /24.
    (( a > 0 && a != 127 && a < 224 && !(a == 169 && b == 254) ))
}

release_mask() {
    if [[ "$CREATED_NGINX_MASK" == 1 ]]; then
        # Some systemd releases refuse disable while masked. Repeat after unmask.
        systemctl disable "$DEFAULT_SERVICE" >/dev/null 2>&1 || true
        systemctl unmask --runtime "$DEFAULT_SERVICE" >/dev/null
        CREATED_NGINX_MASK=0
        if [[ -x /usr/sbin/nginx ]]; then
            systemctl disable "$DEFAULT_SERVICE" >/dev/null 2>&1 || {
                printf 'WARNING: could not disable the default nginx unit; inspect it before reboot.\n' >&2
                return 1
            }
        fi
    fi
}

finish() {
    local result=$?
    trap - EXIT
    if ! release_mask; then result=1; fi
    if [[ "$result" != 0 ]]; then
        if [[ "$STARTED" == 1 ]]; then systemctl stop gateway.service gateway-proxy.service >/dev/null 2>&1 || true; fi
        printf 'Installation stopped. Existing services and project data were not deleted.\n' >&2
        printf 'Review journalctl and the installation paths before retrying.\n' >&2
    fi
    exit "$result"
}
trap finish EXIT

[[ -n "$GATEWAY_IP" ]] || die 'Set GATEWAY_IP explicitly to the IPv4 address assigned to this host on your VPN before first install.'
valid_vpn_ipv4 "$GATEWAY_IP" || die 'GATEWAY_IP must be an IPv4 unicast address; 0/8, loopback, link-local, multicast and reserved addresses are not allowed.'
[[ "$MANAGEMENT_LISTEN" == "$GATEWAY_IP:18080" || "$MANAGEMENT_LISTEN" == '127.0.0.1:18080' ]] || die 'MANAGEMENT_LISTEN must use GATEWAY_IP or 127.0.0.1 on port 18080; wildcard and other addresses are not allowed.'
[[ "${EUID:-$(id -u)}" == 0 ]] || die 'Run this installer as root.'
[[ "$#" == 1 ]] || die "Usage: bash $0 /absolute/path/to/gateway-linux-arm64"
[[ "$(uname -m)" == aarch64 ]] || die 'This installer targets Linux ARM64 only.'
[[ -d /run/systemd/system ]] || die 'systemd is required.'
command -v apt-get >/dev/null || die 'A Debian-compatible apt installation is required.'
command -v ip >/dev/null || die 'iproute2 is required.'
command -v ss >/dev/null || die 'iproute2 (ss) is required.'
command -v pgrep >/dev/null || die 'procps (pgrep) is required.'
[[ -f "$SCRIPT_DIR/verify-release.py" && ! -L "$SCRIPT_DIR/verify-release.py" ]] || die 'Release verifier is missing or unsafe.'

BINARY="$(realpath -e -- "$1")"
[[ -f "$BINARY" && ! -L "$1" ]] || die 'Provide a regular, non-symlink release binary.'
# Never create a fresh gateway next to existing or partially removed data.
# Older installations must be upgraded in place with replace.sh; changing the
# default name is not permission to move certificates or reset credentials.
for existing in /opt/w103d-gateway /var/lib/w103d-gateway /var/cache/w103d-gateway \
    /etc/default/w103d-gateway /etc/systemd/system/w103d-gateway.service \
    /etc/systemd/system/w103d-gateway-proxy.service /etc/logrotate.d/w103d-gateway \
    /etc/logrotate.d/w103d-gateway-traffic; do
    [[ ! -e "$existing" && ! -L "$existing" ]] || die "Legacy installation detected at $existing. Use replace.sh to update it in place; no data was moved or changed."
done
for existing in "$INSTALL_DIR" "$DATA_DIR" /var/cache/gateway \
    /etc/default/gateway /etc/systemd/system/gateway.service \
    /etc/systemd/system/gateway-proxy.service /etc/logrotate.d/gateway "$TRAFFIC_LOGROTATE"; do
    [[ ! -e "$existing" && ! -L "$existing" ]] || die "Gateway installation data already exists at $existing; use replace.sh or inspect the incomplete installation. Nothing was overwritten."
done
[[ -f "$SCRIPT_DIR/gateway-traffic.logrotate" && -r "$SCRIPT_DIR/gateway-traffic.logrotate" && ! -L "$SCRIPT_DIR/gateway-traffic.logrotate" ]] || die 'The traffic logrotate template is missing or unsafe.'

if systemctl is-active --quiet "$DEFAULT_SERVICE"; then
    die 'The existing default nginx service is active. No service was stopped or taken over.'
fi
if pgrep -x nginx >/dev/null; then
    die 'An existing nginx process is running. Review its owner/configuration; no process was stopped.'
fi
if systemctl is-enabled --quiet "$DEFAULT_SERVICE" 2>/dev/null; then
    die 'An existing nginx service is enabled. Review its use before installing this isolated gateway.'
fi
if [[ -n "$(ss -H -lnt '( sport = :18080 or sport = :18081 )')" ]]; then
    die 'TCP 18080 or 18081 is occupied. No existing listener was modified.'
fi
if ! ip -4 -o address show | awk -v expected="$GATEWAY_IP" '{split($4, address, "/"); if (address[1] == expected) found=1} END {exit !found}'; then
    die 'The configured VPN address is not present on a local interface. Fix the tunnel or set GATEWAY_IP to the address assigned to this host before first install.'
fi

if [[ ! -x /usr/sbin/nginx ]]; then
    info 'Installing Nginx with its default service prevented from starting.'
    # Preserve masks that were already present; remove only the mask we create.
    mask_state="$(systemctl is-enabled "$DEFAULT_SERVICE" 2>/dev/null || true)"
    if [[ "$mask_state" != masked && "$mask_state" != masked-runtime ]]; then
        systemctl mask --runtime "$DEFAULT_SERVICE"
        CREATED_NGINX_MASK=1
    fi
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nginx libnginx-mod-http-js logrotate python3 curl ca-certificates
    release_mask
fi
if ! command -v logrotate >/dev/null; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends logrotate
fi
[[ -x /usr/sbin/nginx ]] || die 'Nginx binary was not installed.'
[[ -f /usr/lib/nginx/modules/ngx_http_js_module.so ]] || die 'The matching libnginx-mod-http-js package is required for cookie isolation; install it before continuing.'
command -v python3 >/dev/null || die 'python3 is required for release verification.'
command -v curl >/dev/null || die 'curl is required for release verification.'
command -v logrotate >/dev/null || die 'logrotate was not installed; refusing an unbounded traffic log.'
logrotate --debug "$SCRIPT_DIR/gateway-traffic.logrotate" >/dev/null 2>&1 || die 'The traffic logrotate template failed validation.'
if systemctl is-active --quiet "$DEFAULT_SERVICE" || pgrep -x nginx >/dev/null; then
    die 'Unexpected nginx process detected after package installation. Inspect it manually; no process was stopped.'
fi

install -d -m 0755 -o root -g root "$INSTALL_DIR"
install -d -m 0700 -o root -g root "$DATA_DIR"
install -d -m 0755 -o root -g root /var/cache/gateway
install -m 0755 -o root -g root "$BINARY" "$INSTALL_DIR/gateway"
install -m 0600 -o root -g root "$SCRIPT_DIR/gateway.env" /etc/default/gateway
sed -i -e "s/^GATEWAY_IP=.*/GATEWAY_IP=$GATEWAY_IP/" \
    -e "s/^MANAGEMENT_LISTEN=.*/MANAGEMENT_LISTEN=$MANAGEMENT_LISTEN/" /etc/default/gateway
install -m 0644 -o root -g root "$SCRIPT_DIR/gateway.service" /etc/systemd/system/gateway.service
install -m 0644 -o root -g root "$SCRIPT_DIR/gateway-proxy.service" /etc/systemd/system/gateway-proxy.service
install -m 0644 -o root -g root "$SCRIPT_DIR/gateway.logrotate" /etc/logrotate.d/gateway
install -m 0644 -o root -g root "$SCRIPT_DIR/gateway-traffic.logrotate" "$TRAFFIC_LOGROTATE"

"$INSTALL_DIR/gateway" render \
    --listen "$MANAGEMENT_LISTEN" --gateway-ip "$GATEWAY_IP" \
    --data-dir "$DATA_DIR" --nginx /usr/sbin/nginx
/usr/sbin/nginx -t -c "$DATA_DIR/managed-nginx.conf"
systemctl daemon-reload
systemctl enable gateway-proxy.service gateway.service
STARTED=1
systemctl start gateway-proxy.service
systemctl start gateway.service
systemctl is-active --quiet gateway-proxy.service || die 'The isolated proxy did not become active.'
systemctl is-active --quiet gateway.service || die 'The manager did not become active.'
python3 "$SCRIPT_DIR/verify-release.py" "$INSTALL_DIR/gateway" "$DATA_DIR" "$MANAGEMENT_LISTEN" "$(sha256sum "$BINARY" | awk '{print $1}')" || die 'Installation failed its health/build/generation gate; inspect the stopped release before retrying.'

info 'Gateway installed on the configured VPN address. No firewall, VPN, DNS or certd configuration was changed.'
info "Management: http://$MANAGEMENT_LISTEN/"
info 'Initial password is generated by the program in /var/lib/gateway/initial-password (root-only).'
info 'Read the password privately on the gateway host. Do not paste it into logs, screenshots or chat.'
