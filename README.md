# 🎬 Video Browser

A self-hosted, low-bandwidth video library browser with a built-in clip editor. Built with Go + vanilla HTML/JS — no frameworks, no dependencies beyond ffmpeg.

---

## ✨ Features

### 📁 File Browser
- Browse your video library by folder with a clean, Google Drive-style UI
- Sortable columns: name, modified date, file size
- Password-protected login with session cookies

### 🎞️ Hover Previews
- Animated thumbnail strip on mouse hover (desktop) and tap (mobile)
- 150 frames sampled evenly across the video, displayed as a 10-second CSS loop
- Fully configurable: resolution (160×90 → 480×270), frame count, quality, loop duration
- Generated server-side with ffmpeg — **no video is ever downloaded to the browser**

### 🗂️ Sprite Sheets (for the Clip Editor)
- Sprite sheets cached per video for low-bandwidth timeline scrubbing
- Configurable resolution, frame interval, and grid size
- Visible generation queue in the sidebar with live progress and ETA
- Build per-file, per-folder, or all videos at once

### ✂️ Clip Editor
- Open any video in the sprite-driven editor — still no video downloaded
- Scrubber, In/Out point sliders, filmstrip strip
- Keyboard shortcuts: `I` / `O` to set points, `←` / `→` to step (hold `Shift` for 10s)
- Creates clips server-side with `ffmpeg -c copy` (no re-encoding, keyframe-accurate)
- Clips appear in a virtual **CLIPS** folder inside the source folder

### 🔧 File Actions (per-file ⋮ menu)
- **Play** in PotPlayer (external)
- **Download** directly
- **Edit / Clip** — opens the sprite editor
- **Build Sprites** — queues sprite sheet generation
- **Copy URL** — copies the streamable HTTP URL
- **Copy VM Path** — copies the absolute filesystem path
- **Recover File** — attempts ffmpeg stream-copy recovery for corrupt files
- **Delete** — with confirmation (clips only on read-only mounts)

### ⚠️ Broken File Detection
- Files with missing moov atom (interrupted recordings) are automatically detected
- Shown with a red **!** badge and struck-through name
- One-click recovery tries escalating ffmpeg strategies

### ⚙️ Configurable Settings (sidebar)
- **Preview settings**: resolution, frame count, JPEG quality, loop duration
- **Sprite settings**: resolution, frame interval, grid size
- All settings stored in `localStorage`, applied to all future builds

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
| `--dir` | `./videos` | Root folder to browse |
| `--port` | `2354` | HTTP port to listen on |
| `--cache` | `./thumbcache` | Where sprite sheets and previews are cached |
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

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now video-browser
```

### Reverse proxy (nginx)

```nginx
server {
    listen 80;
    server_name your-domain.com;

    location / {
        proxy_pass http://localhost:2354;
        proxy_set_header Host $host;
        proxy_buffering off;  # important for video streaming
    }
}
```

### Video directory permissions

The `--dir` folder must be readable by the process user. The `--cache` and `--clips` folders must be writable. If your video directory is on a mounted drive owned by root:

```bash
sudo chown -R $USER:$USER /path/to/your/videos
```

---

## 🔒 Security Notes

- Single shared password (configured in `main.go` → `hardcodedPassword`)
- Sessions are in-memory and expire after 24 hours; they are lost on server restart
- All file-serving paths are validated against the root to prevent path traversal
- The snap-confined ffmpeg cannot write to `/mnt/` paths by default — clips and cache are stored locally under the app directory

---

## 📦 What's Stored Where

| Path | Contents |
|---|---|
| `--cache` (default `./thumbcache`) | Sprite sheets (`sprite_*.jpg`, `meta.json`), hover preview strips (`preview.jpg`, `preview_meta.json`), broken file markers |
| `--clips` (default `./clips`) | Clips created by the editor, mirroring the source folder structure |

Cache files are generated on demand and safe to delete — they will be regenerated on next use.
