#!/usr/bin/env bash
# Replace an existing gateway installation (the first-install-only
# install.sh refuses to run twice). Never touches firewall, EasyTier, DNS,
# certd, or the system default nginx unit.
#
#   bash replace.sh /absolute/path/to/gateway-linux-arm64 [--replace-config] [--reset-data] [--dry-run]
#
# Both gateway and legacy w103d-gateway layouts are recognized. Existing paths
# are preserved; this script never silently migrates one layout to the other.
# Existing environment and systemd units are preserved by default. Only the
# explicit --replace-config option installs the adjacent configuration templates.
# The feature-owned traffic logrotate file is always installed separately;
# existing access/error logrotate configuration is never replaced here.
# Without --reset-data the script stops before changing anything when the
# on-device state file is neither schema 4, 5 nor 6. Supported legacy files are
# checked read-only first and explicitly migrated only after a stopped backup.
# --reset-data deletes the device state
# (services, certificates, DNS credentials); the backup taken beforehand is
# the way back.
set -Eeuo pipefail
umask 077

APP_NAME=gateway
INSTALL_DIR=/opt/gateway
DATA_DIR=/var/lib/gateway
CACHE_DIR=/var/cache/gateway
ENV_FILE=/etc/default/gateway
UNIT_MANAGER=/etc/systemd/system/gateway.service
UNIT_PROXY=/etc/systemd/system/gateway-proxy.service
TRAFFIC_LOGROTATE=/etc/logrotate.d/gateway-traffic
MANAGER=gateway.service
PROXY=gateway-proxy.service
LEGACY_INSTALL_DIR=/opt/w103d-gateway
LEGACY_DATA_DIR=/var/lib/w103d-gateway
LEGACY_CACHE_DIR=/var/cache/w103d-gateway
LEGACY_ENV_FILE=/etc/default/w103d-gateway
LEGACY_UNIT_MANAGER=/etc/systemd/system/w103d-gateway.service
LEGACY_UNIT_PROXY=/etc/systemd/system/w103d-gateway-proxy.service
LEGACY_TRAFFIC_LOGROTATE=/etc/logrotate.d/w103d-gateway-traffic
SCHEMA_VERSION=6

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
RESET_DATA=0
REPLACE_CONFIG=0
DRY_RUN=0
STOPPED=0
REPLACED=0
BACKUP=''
BACKUP_READY=0
TRAFFIC_LOGROTATE_EXISTED=0

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
info() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }

installation_present() {
    local target
    for target in "$@"; do
        if [[ -e "$target" || -L "$target" ]]; then return 0; fi
    done
    return 1
}

select_installation() {
    local current=0 legacy=0
    installation_present "$INSTALL_DIR" "$DATA_DIR" "$CACHE_DIR" "$ENV_FILE" "$UNIT_MANAGER" "$UNIT_PROXY" "$TRAFFIC_LOGROTATE" && current=1
    installation_present "$LEGACY_INSTALL_DIR" "$LEGACY_DATA_DIR" "$LEGACY_CACHE_DIR" "$LEGACY_ENV_FILE" "$LEGACY_UNIT_MANAGER" "$LEGACY_UNIT_PROXY" "$LEGACY_TRAFFIC_LOGROTATE" && legacy=1
    if [[ "$current" == 1 && "$legacy" == 1 ]]; then
        die 'Both gateway and legacy w103d-gateway installation paths exist. Resolve the ambiguous layouts manually; nothing was changed.'
    fi
    if [[ "$legacy" == 1 ]]; then
        APP_NAME=w103d-gateway
        INSTALL_DIR="$LEGACY_INSTALL_DIR"
        DATA_DIR="$LEGACY_DATA_DIR"
        CACHE_DIR="$LEGACY_CACHE_DIR"
        ENV_FILE="$LEGACY_ENV_FILE"
        UNIT_MANAGER="$LEGACY_UNIT_MANAGER"
        UNIT_PROXY="$LEGACY_UNIT_PROXY"
        TRAFFIC_LOGROTATE="$LEGACY_TRAFFIC_LOGROTATE"
        MANAGER=w103d-gateway.service
        PROXY=w103d-gateway-proxy.service
        info 'Legacy installation detected: upgrading its existing paths and units in place, without renaming or resetting data.'
    fi
}

print_rollback() {
    local remove_added=''
    info "Rollback: systemctl stop $MANAGER $PROXY"
    info "          mv -- '$DATA_DIR' '$DATA_DIR.manual-rollback-$STAMP' && mv -- '$INSTALL_DIR' '$INSTALL_DIR.manual-rollback-$STAMP'"
    if [[ "$TRAFFIC_LOGROTATE_EXISTED" == 0 ]]; then
        remove_added=" && rm -f -- '$TRAFFIC_LOGROTATE'"
    fi
    info "          tar -xzf '$BACKUP' -C /$remove_added && systemctl daemon-reload && systemctl start $PROXY $MANAGER"
    info "          python3 '$SCRIPT_DIR/verify-release.py' '$INSTALL_DIR/$APP_NAME' '$DATA_DIR' '<restored MANAGEMENT_LISTEN>' '$CURRENT_SHA' '$MANAGER' '$PROXY'"
}

restore_on_failure() {
    local result=$?
    trap - EXIT
    if [[ "$result" != 0 && "$REPLACED" == 1 && "$BACKUP_READY" == 1 ]]; then
        warn 'Release failed. Restoring the complete matching installation and data snapshot.'
        if restore_snapshot; then
            warn 'Previous snapshot restored and verified. This deployment still failed.'
        else
            systemctl stop "$MANAGER" "$PROXY" >/dev/null 2>&1 || true
            warn 'ROLLBACK FAILED: services stopped; retain the backup and failed snapshot for manual recovery.'
        fi
    fi
    if [[ "$result" != 0 && "$STOPPED" == 1 && "$REPLACED" == 0 ]]; then
        warn 'Binary, configuration and state were not replaced. Restarting the previous installation.'
        systemctl start "$PROXY" >/dev/null 2>&1 || true
        systemctl start "$MANAGER" >/dev/null 2>&1 || true
    fi
    # Only worth saying once something on the device has actually been touched. A
    # pre-flight abort (unusable binary, wrong schema) already said "nothing was
    # changed" in its own message, and a recovery banner there would just teach the
    # operator to skim past the one that matters.
    if [[ "$result" != 0 && ( "$STOPPED" == 1 || "$REPLACED" == 1 ) ]]; then
        [[ -z "$BACKUP" ]] || warn "Backup (if it completed) is at $BACKUP"
        warn 'Replaced nothing further. Review journalctl and the paths above before retrying.'
        if [[ "$BACKUP_READY" == 1 ]]; then print_rollback; fi
    fi
    exit "$result"
}

restore_snapshot() {
    systemctl stop "$MANAGER" "$PROXY" || return 1
    # Preserve failed artifacts instead of overlaying old state onto new files.
    # Both roots are fixed, checked non-symlink installation paths above.
    [[ ! -e "$DATA_DIR.failed-$STAMP" && ! -e "$INSTALL_DIR.failed-$STAMP" ]] || return 1
    mv -- "$DATA_DIR" "$DATA_DIR.failed-$STAMP" || return 1
    mv -- "$INSTALL_DIR" "$INSTALL_DIR.failed-$STAMP" || return 1
    tar -xzf "$BACKUP" -C / || return 1
    if [[ "$TRAFFIC_LOGROTATE_EXISTED" == 0 ]]; then rm -f -- "$TRAFFIC_LOGROTATE" || return 1; fi
    systemctl daemon-reload || return 1
    systemctl start "$PROXY" "$MANAGER" || return 1
    local old_listen
    old_listen="$(sed -n 's/^MANAGEMENT_LISTEN=//p' "$ENV_FILE" | head -1)"
    python3 "$SCRIPT_DIR/verify-release.py" "$INSTALL_DIR/$APP_NAME" "$DATA_DIR" "$old_listen" "$CURRENT_SHA" "$MANAGER" "$PROXY" || return 1
}
trap restore_on_failure EXIT

BINARY_ARG=''
for arg in "$@"; do
    case "$arg" in
        --reset-data) RESET_DATA=1 ;;
        --replace-config) REPLACE_CONFIG=1 ;;
        --dry-run)    DRY_RUN=1 ;;
        -*)           die "Unknown option: $arg" ;;
        *)            [[ -z "$BINARY_ARG" ]] || die 'Provide exactly one binary path.'
                      BINARY_ARG="$arg" ;;
    esac
done
[[ -n "$BINARY_ARG" ]] || die "Usage: bash $0 /absolute/path/to/gateway-linux-arm64 [--replace-config] [--reset-data] [--dry-run]"

if [[ "$DRY_RUN" == 0 ]]; then
    [[ "${EUID:-$(id -u)}" == 0 ]] || die 'Run this script as root.'
    [[ "$(uname -m)" == aarch64 ]] || die 'This script targets Linux ARM64 only.'
    [[ -d /run/systemd/system ]] || die 'systemd is required.'
    command -v pgrep >/dev/null || die 'procps (pgrep) is required.'
fi

BINARY=''
if [[ -f "$BINARY_ARG" && ! -L "$BINARY_ARG" ]]; then
    BINARY="$(cd -- "$(dirname -- "$BINARY_ARG")" && pwd -P)/$(basename -- "$BINARY_ARG")"
elif [[ "$DRY_RUN" == 0 ]]; then
    die 'Provide a regular, non-symlink release binary.'
fi

select_installation

[[ -e "$INSTALL_DIR/$APP_NAME" ]] || die "Nothing is installed at $INSTALL_DIR; use install.sh for a first install."
[[ -e "$UNIT_MANAGER" && -e "$UNIT_PROXY" && -e "$ENV_FILE" ]] || \
    die 'The existing installation is incomplete (units or environment file missing); inspect it manually.'
[[ -f "$SCRIPT_DIR/$APP_NAME-traffic.logrotate" && -r "$SCRIPT_DIR/$APP_NAME-traffic.logrotate" && ! -L "$SCRIPT_DIR/$APP_NAME-traffic.logrotate" ]] || \
    die 'A readable, non-symlink traffic logrotate template is required next to this script. Nothing was changed.'
[[ -d "$(dirname -- "$TRAFFIC_LOGROTATE")" && ! -L "$(dirname -- "$TRAFFIC_LOGROTATE")" ]] || die 'The traffic logrotate directory is missing or unsafe.'
if [[ -e "$TRAFFIC_LOGROTATE" || -L "$TRAFFIC_LOGROTATE" ]]; then
    [[ -f "$TRAFFIC_LOGROTATE" && ! -L "$TRAFFIC_LOGROTATE" ]] || die 'Refusing to replace unsafe traffic logrotate configuration.'
    grep -Fxq -e '# Managed by Gateway: traffic logging.' -e '# Managed by W103D Gateway: traffic logging.' "$TRAFFIC_LOGROTATE" || die 'Existing traffic logrotate configuration is not managed by this feature; nothing was changed.'
    TRAFFIC_LOGROTATE_EXISTED=1
fi
if [[ "$REPLACE_CONFIG" == 1 ]]; then
    for template in "$APP_NAME.env" "$APP_NAME.service" "$APP_NAME-proxy.service"; do
        [[ -f "$SCRIPT_DIR/$template" && -r "$SCRIPT_DIR/$template" && ! -L "$SCRIPT_DIR/$template" ]] || \
            die "--replace-config requires readable, non-symlink templates next to this script: $template. Nothing was changed."
    done
    # Published templates intentionally have no deployment-specific address.
    # Reject an unconfigured replacement before stopping the existing services.
    replacement_gateway="$(sed -n 's/^[[:space:]]*GATEWAY_IP[[:space:]]*=[[:space:]]*//p' "$SCRIPT_DIR/$APP_NAME.env" | tail -n 1)"
    case "${replacement_gateway//[[:space:]]/}" in
        ''|'""'|"''") die "--replace-config requires an explicit GATEWAY_IP in $APP_NAME.env. Fill in the template first. Nothing was changed." ;;
    esac
    for target in "$ENV_FILE" "$UNIT_MANAGER" "$UNIT_PROXY"; do
        [[ -f "$target" && ! -L "$target" ]] || die "Refusing to replace a non-regular or symlink configuration file: $target"
    done
fi
[[ ! -L "$INSTALL_DIR" && ! -L "$DATA_DIR" && ! -L "$CACHE_DIR" ]] || die 'Install/data/cache directories must not be symlinks.'
command -v logrotate >/dev/null || die 'logrotate is required before upgrading; refusing an unbounded traffic log.'
command -v python3 >/dev/null || die 'python3 is required for the release health gate.'
command -v curl >/dev/null || die 'curl is required for the release health gate.'
[[ -f "$SCRIPT_DIR/verify-release.py" && ! -L "$SCRIPT_DIR/verify-release.py" ]] || die 'The release verifier is missing or unsafe.'
[[ -f /usr/lib/nginx/modules/ngx_http_js_module.so ]] || die 'Install the matching libnginx-mod-http-js package before upgrading; cookie isolation is mandatory. Nothing was changed.'
logrotate --debug "$SCRIPT_DIR/$APP_NAME-traffic.logrotate" >/dev/null 2>&1 || die 'The traffic logrotate template failed validation. Nothing was changed.'

# Read the on-device schema before touching anything. The new binary refuses a
# non-matching version instead of migrating, so surfacing that here costs one
# read and saves a start failure that looks like anything but its real cause.
DEVICE_SCHEMA='(no state file)'
if [[ -f "$DATA_DIR/state.json" ]]; then
    DEVICE_SCHEMA="$(sed -n 's/.*"schemaVersion"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' "$DATA_DIR/state.json" | head -1)"
    if [[ -z "$DEVICE_SCHEMA" ]]; then
        # "the file is broken" and "the file predates versioning" send an
        # operator to different places, so do not collapse them into one label.
        if [[ "$(tr -d '[:space:]' < "$DATA_DIR/state.json" | head -c 1)" == '{' ]]; then
            DEVICE_SCHEMA='(no schemaVersion key)'
        else
            DEVICE_SCHEMA='(unreadable)'
        fi
    fi
fi
NEEDS_RESET=0
[[ "$DEVICE_SCHEMA" == "$SCHEMA_VERSION" || "$DEVICE_SCHEMA" == 4 || "$DEVICE_SCHEMA" == 5 ]] || NEEDS_RESET=1

if [[ "$NEEDS_RESET" == 1 && "$RESET_DATA" == 0 ]]; then
    printf '\n' >&2
    die "On-device state is schema '$DEVICE_SCHEMA', this build needs $SCHEMA_VERSION, and --reset-data was not given.
The new binary refuses to start on it rather than migrating it. Decide first:
  * restore a backup that matches the installed version, or
  * re-run with --reset-data to delete the device state and rebuild it here.
Deleted by --reset-data: services, certificate material, DNS credentials and
applied snapshot under $DATA_DIR. A timestamped backup is taken first."
fi

CURRENT_VERSION='('"$INSTALL_DIR"'/'"$APP_NAME"' version not read)'
if [[ -x "$INSTALL_DIR/$APP_NAME" ]]; then
    CURRENT_VERSION="$("$INSTALL_DIR/$APP_NAME" version 2>/dev/null || echo '(unreadable)')"
fi
# A binary that will not execute here will not execute under systemd either, and
# "installed cleanly, then refused to start" is the most expensive way to find out
# that a transfer was truncated. The probe runs a temporary executable copy rather
# than chmod-ing the upload: uploads routinely arrive without their execute bit
# (scp does not preserve it), a dry run must leave the machine exactly as it found
# it, and install(1) never consults the source mode anyway.
PROBE="$(mktemp)"
cp -- "$BINARY" "$PROBE"
chmod 0755 "$PROBE"
NEW_VERSION="$("$PROBE" version 2>/dev/null || true)"
if [[ "$RESET_DATA" == 0 ]]; then "$PROBE" state-check --allow-legacy --data-dir "$DATA_DIR" >/dev/null || { rm -f "$PROBE"; die 'Read-only state preflight failed. Nothing was changed.'; }; fi
rm -f "$PROBE"
[[ -n "$NEW_VERSION" ]] || die "$BINARY does not run on this machine (wrong architecture, or the transfer was truncated). Nothing was changed."
NEW_SHA="$(sha256sum "$BINARY" | awk '{print $1}')"
CURRENT_SHA="$(sha256sum "$INSTALL_DIR/$APP_NAME" | awk '{print $1}')"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP="$DATA_DIR-backup-$STAMP.tar.gz"

info 'Planned changes:'
info "  binary      $CURRENT_VERSION  ->  $NEW_VERSION"
info "  sha256      $NEW_SHA"
info "  install     $INSTALL_DIR/$APP_NAME"
info "  traffic log UPDATE feature-owned $TRAFFIC_LOGROTATE (existing access/error rotation preserved)"
if [[ "$REPLACE_CONFIG" == 1 ]]; then
    info "  units       REPLACE from $SCRIPT_DIR: $UNIT_MANAGER, $UNIT_PROXY"
    info "  env         REPLACE from $SCRIPT_DIR: $ENV_FILE"
    warn '--replace-config overwrites device-specific settings. Review the templates before continuing; systemd drop-ins are not changed.'
else
    info "  units       PRESERVE $UNIT_MANAGER, $UNIT_PROXY (including enablement)"
    info "  env         PRESERVE $ENV_FILE"
fi
info "  backup      $BACKUP"
info "  state       schema $DEVICE_SCHEMA"
if [[ "$RESET_DATA" == 0 && "$DEVICE_SCHEMA" != "$SCHEMA_VERSION" ]]; then
    info "  migration   explicit offline schema $DEVICE_SCHEMA -> $SCHEMA_VERSION after stopped backup"
fi
if [[ "$RESET_DATA" == 1 ]]; then
    info "  DATA RESET  delete everything under $DATA_DIR after the backup"
fi
info 'Not touched: firewall, EasyTier, DNS records, certd, system default nginx unit.'

if [[ "$DRY_RUN" == 1 ]]; then
    info ''
    info 'Dry run: nothing above was changed.'
    exit 0
fi

info ''
info 'Stopping services...'
systemctl stop "$MANAGER"
STOPPED=1
systemctl stop "$PROXY"
if pgrep -x nginx >/dev/null; then
    die 'An nginx process is still running after stopping the units. Inspect it manually; nothing was replaced.'
fi

info "Backing up to $BACKUP ..."
BACKUP_PATHS=("${INSTALL_DIR#/}" "${DATA_DIR#/}" "${ENV_FILE#/}" "${UNIT_MANAGER#/}" "${UNIT_PROXY#/}")
if [[ "$TRAFFIC_LOGROTATE_EXISTED" == 1 ]]; then BACKUP_PATHS+=("${TRAFFIC_LOGROTATE#/}"); fi
tar -czf "$BACKUP" -C / "${BACKUP_PATHS[@]}" 2>/dev/null || die 'Backup failed; nothing was replaced.'
chmod 0600 "$BACKUP"
tar -tzf "$BACKUP" >/dev/null || die 'The backup archive is unreadable; nothing was replaced.'
BACKUP_READY=1
info "  $(stat -c '%s bytes' "$BACKUP")"

if [[ "$RESET_DATA" == 1 && -d "$DATA_DIR" ]]; then
    info "Deleting device state under $DATA_DIR ..."
    # Deletion can fail halfway through. Never restart the old binary as if
    # nothing changed after any potentially destructive state reset.
    REPLACED=1
    find "$DATA_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
fi

info 'Installing the new release...'
install -d -m 0755 -o root -g root "$INSTALL_DIR"
install -d -m 0700 -o root -g root "$DATA_DIR"
install -d -m 0755 -o root -g root "$CACHE_DIR"
install -m 0755 -o root -g root "$BINARY" "$INSTALL_DIR/$APP_NAME.new"
mv -f "$INSTALL_DIR/$APP_NAME.new" "$INSTALL_DIR/$APP_NAME"
# Set this immediately: a subsequent config install may fail after the binary
# has already changed, and recovery must not claim the previous release is intact.
REPLACED=1
if [[ "$RESET_DATA" == 0 && "$DEVICE_SCHEMA" != "$SCHEMA_VERSION" ]]; then
    info 'Migrating the stopped state with the candidate binary...'
    "$INSTALL_DIR/$APP_NAME" state-migrate --data-dir "$DATA_DIR" >/dev/null || die 'Offline migration failed; complete snapshot rollback follows.'
fi
install -m 0644 -o root -g root "$SCRIPT_DIR/$APP_NAME-traffic.logrotate" "$TRAFFIC_LOGROTATE"

if [[ "$REPLACE_CONFIG" == 1 ]]; then
    install -m 0600 -o root -g root "$SCRIPT_DIR/$APP_NAME.env" "$ENV_FILE"
    install -m 0644 -o root -g root "$SCRIPT_DIR/$APP_NAME.service" "$UNIT_MANAGER"
    install -m 0644 -o root -g root "$SCRIPT_DIR/$APP_NAME-proxy.service" "$UNIT_PROXY"
    systemctl daemon-reload
    # Replacement preserves prior enablement; changing templates is not consent
    # to enable previously disabled units, including during failure rollback.
fi
systemctl start "$PROXY"
systemctl is-active --quiet "$PROXY" || die 'The data plane did not become active. See: journalctl -u '"$PROXY"
systemctl start "$MANAGER"
systemctl is-active --quiet "$MANAGER" || die 'The manager did not become active. See: journalctl -u '"$MANAGER"

LISTEN="$(sed -n 's/^MANAGEMENT_LISTEN=//p' "$ENV_FILE" | head -1)"
info ''
info "Data plane: active   Manager: active   Listen: ${LISTEN:-unknown}"
[[ -n "$LISTEN" ]] || die 'Cannot determine the configured management health endpoint.'
python3 "$SCRIPT_DIR/verify-release.py" "$INSTALL_DIR/$APP_NAME" "$DATA_DIR" "$LISTEN" "$NEW_SHA" "$MANAGER" "$PROXY" || die 'Release verification failed; automatic snapshot rollback follows.'

if [[ "$RESET_DATA" == 1 ]]; then
    info ''
    info "Device state was reset. A new password is in $DATA_DIR/initial-password (root-only)."
    info 'Read it privately on the gateway host. Do not paste it into logs, screenshots or chat.'
fi
info ''
print_rollback
info ''
info 'No public listener, firewall, DNS, certd or EasyTier change was made by this script.'
