#!/bin/sh
set -eu

STATE_DIR="${TS_STATE_DIR:-/var/lib/tailscale}"
STATE_FILE="${STATE_DIR}/tailscaled.state"
SOCKET="/var/run/tailscale/tailscaled.sock"
HOSTNAME="${TS_HOSTNAME:-dumpster}"

mkdir -p "${DATA_DIR:-/data}" "$STATE_DIR" /var/run/tailscale

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

exec /app/dumpster
