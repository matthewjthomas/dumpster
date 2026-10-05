#!/bin/sh
set -eu

STATE_DIR="${TS_STATE_DIR:-/var/lib/tailscale}"
STATE_FILE="${STATE_DIR}/tailscaled.state"
SOCKET="/var/run/tailscale/tailscaled.sock"
HOSTNAME="${TS_HOSTNAME:-dumpster}"
DATA_DIR="${DATA_DIR:-/data}"
PUID="${PUID:-99}"
PGID="${PGID:-100}"

case "$PUID" in
  ''|*[!0-9]*) echo "PUID must be a numeric user ID" >&2; exit 1 ;;
esac
case "$PGID" in
  ''|*[!0-9]*) echo "PGID must be a numeric group ID" >&2; exit 1 ;;
esac

mkdir -p "$DATA_DIR" "$STATE_DIR" /var/run/tailscale

PERMISSION_MARKER="$DATA_DIR/.dumpster-permissions"
EXPECTED_OWNERSHIP="${PUID}:${PGID}"
CURRENT_OWNERSHIP="$(cat "$PERMISSION_MARKER" 2>/dev/null || true)"
if [ "$CURRENT_OWNERSHIP" != "$EXPECTED_OWNERSHIP" ]; then
  echo "Setting Dumpster storage ownership to ${EXPECTED_OWNERSHIP}..."
  chown -R "$EXPECTED_OWNERSHIP" "$DATA_DIR"
  find "$DATA_DIR" -type d -exec chmod 0770 {} +
  find "$DATA_DIR" -type f -exec chmod 0660 {} +
  printf '%s\n' "$EXPECTED_OWNERSHIP" > "$PERMISSION_MARKER"
  chown "$EXPECTED_OWNERSHIP" "$PERMISSION_MARKER"
  chmod 0660 "$PERMISSION_MARKER"
else
  chown "$EXPECTED_OWNERSHIP" "$DATA_DIR"
  chmod 0770 "$DATA_DIR"
fi

tailscaled \
  --tun=userspace-networking \
  --state="$STATE_FILE" \
  --socket="$SOCKET" &

waited=0
while [ ! -S "$SOCKET" ] && [ "$waited" -lt 15 ]; do
  sleep 1
  waited=$((waited + 1))
done

if [ -n "${TS_AUTHKEY:-}" ]; then
  echo "Connecting Dumpster to Tailscale as ${HOSTNAME}..."
  tailscale up --hostname="$HOSTNAME" --authkey="$TS_AUTHKEY" --accept-dns=false
elif [ -f "$STATE_FILE" ]; then
  echo "Reconnecting Dumpster to Tailscale as ${HOSTNAME}..."
  tailscale up --hostname="$HOSTNAME" --accept-dns=false || true
else
  echo "No TS_AUTHKEY or saved Tailscale state found."
  echo "Set TS_AUTHKEY, or run: docker exec -it dumpster tailscale up --hostname=${HOSTNAME}"
fi

if tailscale status --json 2>/dev/null | grep -q '"BackendState": *"Running"'; then
  tailscale serve reset >/dev/null 2>&1 || true
  tailscale serve --bg 8080
  echo "Tailscale Serve is active for ${HOSTNAME}."
else
  echo "Tailscale is not connected. Authenticated requests will be denied."
fi

echo "Starting Dumpster as ${PUID}:${PGID}..."
umask 0007
exec gosu "${PUID}:${PGID}" /app/dumpster
