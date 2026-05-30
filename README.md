# 🎬 Video Browser

A self-hosted, low-bandwidth video library browser with a built-in clip editor. Built with Go + vanilla HTML/JS — no frameworks, no dependencies beyond ffmpeg.

> 🛰️ **Designed for remote / low-bandwidth use.** The full video file is **never** sent to the browser. All scrubbing, previewing, and editing is driven by tiny sprite sheets and thumbnail strips generated server-side. Only when you explicitly **play** or **download** does the video file itself cross the network.

---

## ✨ Features

### 📁 File Browser
- Browse your video library by folder with a clean, Google Drive–style UI
- **Two view modes** (toggle in topbar):
  - **List** — sortable columns with name, modified date, size
  - **Grid** — tile thumbnails with poster images, hover to preview
- **Folder sizes** computed recursively and shown next to each folder
- **Summary bar** under the breadcrumb shows totals (`3 folders · 12 videos · 47.3 GB`)
- **Sortable** by name, modified date, or size in both views
- **Back button** in the breadcrumb (or press `Backspace`)
- **🌙 Dark mode** toggle in the topbar (persisted)

### 🔍 Search
- Filename search across the entire library from the topbar
- Press `/` anywhere to focus the search box
- Live results with 250ms debounce, capped at 200 hits
- All row actions work on search results (play, edit, ⋮ menu, etc.)

### 🎞️ Hover Previews (Grid View)
- **Poster image** (single first frame, ~10–20 KB JPEG) on every tile — generated in 1–3 seconds, cached forever
- **Hover to play**: 150-frame animated strip plays as a 10-second CSS loop
- If the full preview isn't built yet, a progress bar appears on the tile and the animation kicks in when ready
- Fully configurable: resolution, frame count, JPEG quality, loop duration
- **No video downloaded** — only one ~500 KB JPEG strip per video

### 📋 Click-to-Expand (List View)
- Click any video row to expand an inline panel showing:
  - **Animated preview** on the left (with mini playback progress bar)
  - **File info** on the right — resolution, duration, fps, bitrate, codec, audio, container, size
- Click the preview to launch the in-browser player
- Click the same row again (or another) to collapse / switch

### 🗂️ Sprite Sheets (for the Clip Editor)
- Sprite sheets cached per video for low-bandwidth timeline scrubbing
- Configurable resolution, frame interval, and grid size
- **Visible queue in the sidebar** with type badges (Sprite/Preview), live progress, and ETA
- Build per-file, per-folder, or all videos at once

### ✂️ Clip Editor
- Open any video in the sprite-driven editor — still no video downloaded
- Scrubber, In/Out point sliders, filmstrip strip
- **Custom clip name** field (or auto-numbered `<source>_clip01.mp4`)
- Keyboard shortcuts: `I` / `O` to set points, `←` / `→` to step (hold `Shift` for 10s), `Enter` to save
- Creates clips server-side with `ffmpeg -c copy` (no re-encoding, keyframe-accurate)
- Clips appear in a virtual **CLIPS** folder inside the source folder

### 🔧 File Actions (per-file ⋮ menu)
- **File Info** — opens a modal with full ffprobe metadata
- **Play** in PotPlayer (external)
- **Download** directly
- **Edit / Clip** — opens the sprite editor
- **Build Sprites** — queues sprite sheet generation
- **Copy URL** — copies the streamable HTTP URL
- **Copy VM Path** — copies the absolute filesystem path
- **Recover File** — attempts ffmpeg stream-copy recovery for corrupt files
- **Delete** — with confirmation

### ⚠️ Broken File Detection & Recovery
- Files with missing moov atom (interrupted recordings) are auto-detected
- Shown with a red **!** badge and struck-through name
- "Recover File" tries three escalating ffmpeg strategies and saves the result as `<original>_recovered.<ext>`

### ⚙️ Configurable Settings (sidebar)
- **Preview settings**: resolution, frame count, JPEG quality, loop duration
- **Sprite settings**: resolution, frame interval, grid size
- All settings stored in `localStorage`, applied to all future builds

### ⚡ Performance
- `-skip_frame nointra` decoding makes sprite + preview generation **60–150× faster** (keyframes only)
- Two-step preview generation gives real progress instead of a stuck % bar
- Poster generation cached forever (`poster.jpg`)
- All ffmpeg subprocesses tracked and killed cleanly on `SIGTERM` shutdown
- Per-file mutex prevents duplicate work when multiple requests hit the same path

### 🔒 Persistent Sessions
- Sessions saved to `sessions.json` on disk (0600 permissions)
- Restored on startup — no more forced login after server restart or rebuild
- 24-hour TTL per session

### ⌨️ Keyboard Shortcuts
| Key | Action |
|---|---|
| `/` | Focus search box |
| `Esc` | Close menu / modal / expanded row |
| `Backspace` | Navigate up one folder |
| `I` / `O` | Set in / out point (clip editor) |
| `←` / `→` | Step 2s (Shift = 10s) (clip editor) |
| `Enter` | Save clip (when name field focused) |

---

## 🚀 Deployment

### Prerequisites

```bash
# Ubuntu / Debian
sudo apt-get install ffmpeg

# Verify both binaries exist
ffmpeg -version
ffprobe -version
```

> If using the snap version of ffmpeg, `ffprobe` may be at `ffmpeg.ffprobe`. The server detects this automatically.

### Build

```bash
git clone https://github.com/SamirPRO1/video-browser.git
cd video-browser
go build -o video-browser .
```

Requires **Go 1.18+**. No other Go dependencies.

### Run

```bash
./video-browser --dir /path/to/your/videos --port 2354
```

| Flag | Default | Description |
|---|---|---|
| `--dir` | `./videos` | Root folder to browse (read-only OK) |
| `--port` | `2354` | HTTP port to listen on |
| `--cache` | `./thumbcache` | Where sprite sheets, previews, and posters are cached |
| `--clips` | `./clips` | Where created clips are saved |

The server prints the password on startup. Default: `videos123` (change it in `main.go`).

### Run as a systemd service

```ini
# /etc/systemd/system/video-browser.service
[Unit]
Description=Video Browser
After=network.target

[Service]
User=ubuntu
WorkingDirectory=/home/ubuntu/video-browser
ExecStart=/home/ubuntu/video-browser/video-browser --dir /mnt/videos --port 2354
Restart=on-failure
# Send SIGTERM so ffmpeg children are killed cleanly
KillSignal=SIGTERM
TimeoutStopSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now video-browser
```

> Always use SIGTERM (`systemctl stop` or `kill <pid>`) — never SIGKILL (`kill -9`). The server catches SIGTERM and kills all child ffmpeg processes; SIGKILL leaves them orphaned.

### Reverse proxy (nginx)

```nginx
server {
    listen 80;
    server_name your-domain.com;

    # Large video uploads / downloads
    client_max_body_size 8G;

    location / {
        proxy_pass http://localhost:2354;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;       # important for streaming
        proxy_request_buffering off;
        proxy_read_timeout 3600s;  # for long sprite builds
    }
}
```

### Video directory permissions

The `--dir` folder must be readable by the process user. The `--cache` and `--clips` folders must be writable. If your video directory is on a mounted drive owned by root:

```bash
sudo chown -R $USER:$USER /path/to/your/videos
```

> The video directory can be **read-only** — clips never write into it. They go to `--clips` and appear in the browser as a virtual `CLIPS` folder inside each source directory.

---

## 🔒 Security Notes

- Single shared password (configured in `main.go` → `hardcodedPassword`) — change this before exposing to the internet
- Sessions are persisted to `sessions.json` (0600 perms) — exclude from backups if sensitive
- All file-serving paths are validated against the root to prevent path traversal
- The snap-confined ffmpeg cannot write to `/mnt/` paths by default — clips and cache are stored locally under the app directory
- No upload endpoint by design (read-only library + writable clip output)

---

## 📦 What's Stored Where

| Path | Contents |
|---|---|
| `--dir` | Your source video library (untouched, can be read-only) |
| `--cache` (default `./thumbcache`) | Per-video subdirectories with: `sprite_*.jpg` + `meta.json` (sprite sheets), `preview.jpg` + `preview_meta.json` (hover animation strip), `poster.jpg` (grid thumbnail), `broken` marker file if applicable |
| `--clips` (default `./clips`) | Clips created by the editor, mirroring the source folder structure |
| `./sessions.json` | Persistent login tokens (auto-managed, gitignored) |

Cache files are generated on demand and safe to delete — they will regenerate on next use.

---

## 🧪 How Generation Works (under the hood)

| Artifact | Command (simplified) | When |
|---|---|---|
| **Poster** (`poster.jpg`) | `ffmpeg -ss 1 -i src -vf scale -frames:v 1 poster.jpg` | First request to `/api/poster` |
| **Preview strip** (`preview.jpg`) | Step 1: extract N keyframes individually → Step 2: tile vertically | First hover (list view expand) or "Generate All Previews" |
| **Sprite sheets** (`sprite_*.jpg`) | `ffmpeg -skip_frame nointra -i src -vf "fps=1/N,tile=12x12"` | First open in editor or "Build Sprites" |
| **Clip** | `ffmpeg -ss S -i src -t D -c copy out.mp4` | "Create Clip" in editor |

All long-running generations report progress via `-progress pipe:1` parsed live by the Go server, exposed at `/api/queue` for the sidebar.

---

## 📡 API Reference

All endpoints require a valid `session` cookie except `/login`.

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/files?path=` | List directory entries |
| `DELETE` | `/api/files?path=` | Delete a file |
| `GET` | `/api/search?q=` | Search videos by filename |
| `GET` | `/api/info?path=` | ffprobe metadata for one file |
| `GET` | `/api/poster?path=` | Single first-frame JPEG (lazy generated) |
| `GET` | `/api/preview?path=` | Hover animation strip JPEG |
| `GET` | `/api/preview/meta?path=` | Strip metadata (frames, width, height) |
| `POST` | `/api/preview/build?path=&...` | Queue preview generation |
| `GET` | `/api/preview/progress?path=` | Progress for one preview |
| `POST/GET` | `/api/preview/build-all` | Bulk preview generation / progress |
| `GET` | `/api/sprite?path=&sheet=` | Sprite sheet image |
| `POST` | `/api/sprite/build?path=&...` | Queue sprite generation |
| `GET` | `/api/sprite/progress?path=` | Progress for one sprite job |
| `POST` | `/api/sprite/build-folder?path=&...` | Queue all videos in folder |
| `POST/GET` | `/api/sprite/build-all` | Bulk sprite generation / progress |
| `GET` | `/api/sprite/queue` | All sprite jobs |
| `GET` | `/api/queue` | Unified sprite + preview queue |
| `GET` | `/api/video/meta?path=` | Sprite sheets + metadata (auto-generates if missing) |
| `POST` | `/api/clip` | Body: `{path, start, end, name?}` — create clip |
| `POST` | `/api/recover?path=` | Attempt ffmpeg recovery of broken file |
| `GET` | `/files/<path>` | Serve video file (CLIPS routed to clips dir) |

See [dev.md](./dev.md) for more details on the architecture and how to extend.
