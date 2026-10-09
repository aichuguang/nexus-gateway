#!/usr/bin/env bash
# Build the Linux/arm64 binary on this machine and hand it to replace.sh on the
# device. This is the host-side half: replace.sh is the device-side half and
# must sit next to the binary it installs.
#
#   GATEWAY_HOST=gateway.example.com bash deploy/push.sh [--replace-config] [--reset-data] [--dry-run]
#
# An optional password is read from GATEWAY_SSH_PASS, never from an argument.
# Use a silent prompt to set it: typing a literal assignment can leak to shell
# history. SSH keys or an interactive prompt are preferred. --reset-data is passed through to
# replace.sh and deletes the device state; the backup replace.sh takes first is
# the way back. Existing units/env are kept unless --replace-config is supplied.
# --dry-run builds/uploads and runs the remote preflight without replacing the
# installed release (it is not an offline or no-network preview).
set -Eeuo pipefail

REPO="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
HOST="${GATEWAY_HOST:-${W103D_HOST:-}}"
[[ -n "$HOST" && "$HOST" =~ ^[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || {
  echo 'ERROR: Set GATEWAY_HOST to an explicit SSH hostname or IPv4 address. No default deployment target is selected.' >&2
  exit 1
}
# Legacy environment variables remain accepted for existing automation.
GATEWAY_SSH_PASS="${GATEWAY_SSH_PASS:-${W103D_SSH_PASS:-}}"
REMOTE_DIR=/tmp/gateway-deploy
BINARY=gateway-linux-arm64
REPLACE_CONFIG=0
for arg in "$@"; do
  case "$arg" in
    --replace-config) REPLACE_CONFIG=1 ;;
    --reset-data|--dry-run) ;;
    *) printf 'ERROR: Unknown option: %s\n' "$arg" >&2; exit 1 ;;
  esac
done
TRAFFIC_TEMPLATES=(gateway-traffic.logrotate w103d-gateway-traffic.logrotate)
CONFIG_TEMPLATES=(gateway.env gateway.service gateway-proxy.service w103d-gateway.env w103d-gateway.service w103d-gateway-proxy.service)
[[ -f "$REPO/deploy/verify-release.py" && ! -L "$REPO/deploy/verify-release.py" ]] || { echo 'ERROR: release verifier is missing or unsafe.' >&2; exit 1; }
for template in "${TRAFFIC_TEMPLATES[@]}"; do
  [[ -f "$REPO/deploy/$template" && -r "$REPO/deploy/$template" && ! -L "$REPO/deploy/$template" ]] || {
    printf 'ERROR: A readable, non-symlink traffic logrotate template is required: %s\n' "$template" >&2
    exit 1
  }
done
if [[ "$REPLACE_CONFIG" == 1 ]]; then
  for template in "${CONFIG_TEMPLATES[@]}"; do
    [[ -f "$REPO/deploy/$template" && -r "$REPO/deploy/$template" && ! -L "$REPO/deploy/$template" ]] || {
      printf 'ERROR: --replace-config requires a readable, non-symlink template: %s\n' "$template" >&2
      exit 1
    }
  done
fi

# Password auth needs a program to type the password, and ssh only consults that
# program when it has no terminal to ask on. Both are set here rather than left
# to the caller so the script behaves the same however it is invoked.
if [[ -n "$GATEWAY_SSH_PASS" ]]; then
  ASKPASS="$(mktemp)"
  trap 'rm -f "$ASKPASS"' EXIT
  # A quoted heredoc so the file holds a literal backslash-n rather than a
  # newline inside the format string, and so the password is an argument to
  # printf instead of part of the format: a % or a backslash in it then cannot
  # be read as anything but a character.
  cat >"$ASKPASS" <<'ASKPASS_EOF'
#!/bin/sh
printf '%s\n' "$GATEWAY_SSH_PASS"
ASKPASS_EOF
  chmod 700 "$ASKPASS"
  export GATEWAY_SSH_PASS SSH_ASKPASS="$ASKPASS" SSH_ASKPASS_REQUIRE=force DISPLAY="${DISPLAY:-:0}"
fi
SSH=(ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 "root@$HOST")
SCP=(scp -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"; [[ -z "${ASKPASS:-}" ]] || rm -f "$ASKPASS"' EXIT

echo "building $BINARY and its UI/provenance from $REPO"
node "$REPO/scripts/build.mjs" --out-dir "$STAGE"
cp "$REPO/deploy/replace.sh" "$STAGE/"
cp "$REPO/deploy/verify-release.py" "$STAGE/"
UPLOADS=("$STAGE/$BINARY" "$STAGE/replace.sh" "$STAGE/verify-release.py" "$STAGE/build-manifest.json" "$STAGE/SHA256SUMS")
for template in "${TRAFFIC_TEMPLATES[@]}"; do
  cp "$REPO/deploy/$template" "$STAGE/"
  UPLOADS+=("$STAGE/$template")
done
if [[ "$REPLACE_CONFIG" == 1 ]]; then
  for template in "${CONFIG_TEMPLATES[@]}"; do
    cp "$REPO/deploy/$template" "$STAGE/"
    UPLOADS+=("$STAGE/$template")
  done
fi

# Verify exactly the selected payload; optional templates may be preserved.
: > "$STAGE/SHA256SUMS"
for file in "${UPLOADS[@]}"; do
  [[ "${file##*/}" == SHA256SUMS ]] || (cd "$STAGE" && sha256sum "${file##*/}" >> SHA256SUMS)
done
LOCAL_SHA="$(sha256sum "$STAGE/$BINARY" | cut -d' ' -f1)"
echo "local sha256 $LOCAL_SHA"

"${SSH[@]}" "mkdir -p $REMOTE_DIR"
"${SCP[@]}" "${UPLOADS[@]}" "root@$HOST:$REMOTE_DIR/"

REMOTE_SHA="$("${SSH[@]}" "sha256sum $REMOTE_DIR/$BINARY | cut -d' ' -f1")"
# A transfer that silently truncated would otherwise be installed and then
# reported as a bad build. Comparing here names the transfer as the culprit.
if [[ "$REMOTE_SHA" != "$LOCAL_SHA" ]]; then
  echo "transfer mismatch: local $LOCAL_SHA, device $REMOTE_SHA" >&2
  exit 1
fi
"${SSH[@]}" "cd $REMOTE_DIR && sha256sum -c SHA256SUMS"

# printf with a format and no arguments prints the format once, filling %q with
# nothing, so an unconditional "$(printf '%q ' "$@")" appends a quoted empty
# string and replace.sh sees two paths. Only expand when there is something to
# expand.
EXTRA=''
if [[ $# -gt 0 ]]; then EXTRA="$(printf '%q ' "$@")"; fi
"${SSH[@]}" "cd $REMOTE_DIR && bash replace.sh $REMOTE_DIR/$BINARY $EXTRA"
