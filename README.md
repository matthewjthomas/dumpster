# Dumpster

A small, self-hosted file drop. Dumpster exposes a web UI and HTTP upload endpoint, writes files directly to a mounted volume, and uses Tailscale Serve as its primary access and identity layer.

## What it does

- Drag-and-drop, multi-file, and whole-folder web uploads
- Per-file upload progress
- Persistent, resumable 32 MiB chunk uploads with pause and cancellation
- Browser-configurable parallel uploads (1–32, remembered per browser)
- `multipart/form-data` HTTP upload endpoint
- File listing and downloads
- Collision-safe names (`report.pdf`, `report (1).pdf`, and so on)
- Atomic writes so incomplete uploads never appear as finished files
- Embedded Tailscale in userspace mode; no `NET_ADMIN` capability or TUN device required
- Direct requests are denied by default, even if container port 8080 is accidentally published
- Optional Tailscale login allowlist

Dumpster intentionally does not offer deletion of completed files. Files are ordinary files on the mounted volume; manage them there. Paused, incomplete uploads can be cancelled from the web UI.

## Quick start from source

1. Create a Tailscale auth key at <https://login.tailscale.com/admin/settings/keys>. A reusable key works best with container recreation; an ephemeral key is also supported.
2. Configure and start:

   ```sh
   cp .env.example .env
   # Put your key in .env, then:
   docker compose up -d --build
   ```

3. Find the HTTPS URL:

   ```sh
   docker exec dumpster tailscale serve status
   ```

Open the reported `https://dumpster.<tailnet>.ts.net` URL. Any authenticated member of the tailnet can upload by default. Tailscale ACLs/grants still apply.

The Tailscale node state persists in `./tailscale-state`, so `TS_AUTHKEY` can be removed from `.env` after the first successful connection if desired.

## Published container

Every push to `main` publishes multi-architecture images for AMD64 and ARM64 to GitHub Container Registry:

```sh
docker pull ghcr.io/matthewjthomas/dumpster:latest
```

Version tags such as `v1.2.3` also publish `1.2.3` and `1.2` image tags. Pull requests build and test the image without publishing it.

## Mounting persistent storage

Dumpster needs two persistent folder mappings. In Unraid, add both as read/write Path entries:

- **Uploaded files:** host path `/mnt/user/dumpster` → container path `/data`
- **Tailscale identity:** host path `/mnt/user/appdata/dumpster/tailscale` → container path `/var/lib/tailscale`

`/data` contains completed files and resumable-upload state. `/var/lib/tailscale` preserves the Tailscale machine identity so the container does not need to register as a new device whenever it is recreated.

With Compose, `DUMPSTER_STORAGE` controls the host path mounted at `/data`:

```env
DUMPSTER_STORAGE=/mnt/persistent/dumpster
```

Or edit the Compose volume to use a Docker named volume:

```yaml
volumes:
  - dumpster-files:/data
```

The included Compose file maps `./tailscale-state` to `/var/lib/tailscale`.

## Unraid setup

Dumpster does not require privileged mode, host networking, extra capabilities, or a `/dev/net/tun` mapping. Its embedded Tailscale daemon uses userspace networking.

1. In the Unraid web interface, open **Docker** and select **Add Container**.
2. Configure the container:
   - **Name:** `dumpster`
   - **Repository:** `ghcr.io/matthewjthomas/dumpster:latest`
   - **Network Type:** `Bridge`
   - **Privileged:** Off
3. Add a read/write **Path** for uploaded files:
   - **Container Path:** `/data`
   - **Host Path:** `/mnt/user/dumpster`
4. Add a read/write **Path** for persistent Tailscale state:
   - **Container Path:** `/var/lib/tailscale`
   - **Host Path:** `/mnt/user/appdata/dumpster/tailscale`
5. Add these **Variables**:
   - `TS_AUTHKEY` — a Tailscale auth key from <https://login.tailscale.com/admin/settings/keys>
   - `TS_HOSTNAME` — `dumpster`, or another desired tailnet hostname
   - `PUID` — `99`, the Unraid `nobody` user
   - `PGID` — `100`, the Unraid `users` group
   - `MAX_UPLOAD_MB` — maximum size of one file; `1024` by default or `102400` for 100 GiB
   - `ALLOWED_USERS` — optional comma-separated Tailscale login emails; leave empty for all tailnet members
6. Do not add a host port mapping. Normal access goes through Tailscale Serve, and direct requests to port 8080 are rejected.
7. Select **Apply**, then inspect startup if needed:

   ```sh
   docker logs -f dumpster
   ```

8. Display the HTTPS address:

   ```sh
   docker exec dumpster tailscale serve status
   ```

Open the reported `https://dumpster.<tailnet>.ts.net` address. Once Tailscale has registered successfully, its identity is retained in `/mnt/user/appdata/dumpster/tailscale`; the auth key can then be removed if desired.

To update Dumpster later, select **Check for Updates** in Unraid's Docker page and apply the available image update. The two mapped folders preserve uploaded files, incomplete resumable uploads, and Tailscale identity across upgrades.

Tailscale continues to run as root inside the container, but the Dumpster web application runs as `PUID:PGID`. On the first start—or whenever those IDs change—the entrypoint updates `/data` to use that ownership, `0770` directory permissions, and `0660` file permissions. A `.dumpster-permissions` marker prevents repeating the recursive migration on every restart.

## Resumable uploads

The browser sends files in 32 MiB chunks. If a request fails, the partial file and its current byte offset remain under `/data/.dumpster-uploads`. The UI displays it as **Paused**.

- In the same browser session, click **Resume** to continue immediately.
- After reloading or returning later, click **Resume** and select the original file again. Dumpster verifies its name, size, and modification time before continuing.
- Click **Cancel** on a paused upload to permanently remove its partial data.

Incomplete uploads survive container restarts because their bytes and metadata are stored under `/data`. They are never shown as completed files or offered for download.

## Upload API

Upload one or more fields named `files`. An optional `path` field preserves a relative folder path:

```sh
curl -F "files=@photo.jpg" \
     -F "files=@archive.zip" \
     https://dumpster.example-tailnet.ts.net/api/files

curl -F "path=project/assets/photo.jpg" \
     -F "files=@photo.jpg" \
     https://dumpster.example-tailnet.ts.net/api/files
```

Endpoints:

- `POST /api/files` — upload multipart fields named `files`
- `POST /api/uploads` — create a resumable upload
- `PATCH /api/uploads/{id}` — append a chunk at the supplied `Upload-Offset`
- `GET /api/uploads` — list incomplete uploads
- `DELETE /api/uploads/{id}` — cancel an incomplete upload and delete its partial data
- `GET /api/files` — list uploaded files
- `GET /files/{name}` — download a file
- `GET /api/me` — show the authenticated Tailscale identity
- `GET /health` — unauthenticated container health check

## Configuration

- `TS_AUTHKEY` — Tailscale auth key used to join the tailnet
- `TS_HOSTNAME` — Tailscale machine name; defaults to `dumpster`
- `DUMPSTER_STORAGE` — host-side storage path in Compose; defaults to `./storage`
- `PUID` — numeric user ID used by the web application and uploaded files; defaults to `99`
- `PGID` — numeric group ID used by the web application and uploaded files; defaults to `100`
- `MAX_UPLOAD_MB` — maximum size of one file; defaults to `1024`
- `ALLOWED_USERS` — optional comma-separated Tailscale login emails; empty allows all tailnet members
- `AUTH_BYPASS` — disables authentication when true; development only

## Manual Tailscale login

Instead of an auth key, start the container and authenticate interactively:

```sh
docker compose up -d --build
docker exec -it dumpster tailscale up --hostname=dumpster
docker restart dumpster
```

## Local development

Uncomment `AUTH_BYPASS` and the `ports` section in `docker-compose.yml`, then open <http://localhost:8080>. Never expose this configuration to an untrusted network.

Run tests locally with:

```sh
go test ./...
```
