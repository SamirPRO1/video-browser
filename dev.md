# 🛠️ Developer Guide

Everything you need to understand the codebase, run it locally, and contribute.

---

## 🗺️ Project Overview

Video Browser is a **single-binary Go server** that serves three hand-written HTML pages. There are no frontend frameworks, no build steps, no npm — just Go + vanilla JS.

```
video-browser/
├── main.go          # Entire backend (~1400 lines)
├── index.html       # File browser UI
├── editor.html      # Sprite-driven clip editor
├── login.html       # Login page
├── go.mod           # Go module (no external dependencies)
├── thumbcache/      # Generated sprites + previews (gitignored)
├── clips/           # Created clips (gitignored)
└── videos/          # Default video root for local dev
```

---

## 🏗️ Architecture

### Backend (`main.go`)

The server is organized into clearly separated sections:

| Section | What it does |
|---|---|
| **Config & flags** | CLI flags: `--dir`, `--port`, `--cache`, `--clips` |
| **Session store** | In-memory map of token → expiry, 24h TTL |
| **Auth middleware** | `requireAuth()` wraps handlers, checks cookie |
| **File listing** | `listHandler` — reads dir, injects virtual `CLIPS` folder |
| **Sprite config** | `spriteConfig` struct + `spriteConfigFromQuery()` |
| **Sprite generation** | `generateSprites()` (sync) + `generateSpritesAsync()` (goroutine) |
| **Sprite job tracking** | In-memory map, polled by frontend via `/api/sprite/queue` |
| **Preview config** | `previewConfig` struct + `previewConfigFromQuery()` |
| **Preview generation** | `generatePreviewAsync()` — two-step: frames then tile |
| **Preview job tracking** | Same pattern as sprites |
| **Unified queue** | `/api/queue` merges both job maps |
| **Bulk jobs** | `runBulkSprite()` / `runBulkPreview()` — sequential, one at a time |
| **Clip editor** | `metaHandler`, `spriteHandler`, `clipHandler` |
| **Recovery** | `recoverHandler` — tries 3 ffmpeg strategies |
| **File serving** | `/files/` — transparent CLIPS path routing |

### Frontend pages

| Page | Route | Description |
|---|---|---|
| `index.html` | `/` | File browser + sidebar controls |
| `editor.html` | `/editor?path=...` | Sprite-driven clip editor |
| `login.html` | `/login` | Password form |

All three pages are served directly from disk with `http.ServeFile` — edit and refresh, no restart needed for HTML/JS/CSS changes.

### Key design decisions

- **No video to browser**: sprites and previews are the only images sent. The full video file is only fetched when the user explicitly plays it or downloads it.
- **Lazy + cached generation**: nothing is generated until first use. Results are written to `--cache` and reused forever (with mtime-based invalidation for sprites).
- **Virtual CLIPS folder**: clips are stored in `--clips/<rel-dir>/` locally but appear in the browser as `/<rel-dir>/CLIPS/`. `splitAtClips()` in `main.go` handles the routing transparently.
- **Sequential bulk jobs**: bulk sprite/preview generation runs one video at a time to avoid saturating ffmpeg. A future improvement would be a configurable concurrency level.

---

## 🧰 Local Development

### Requirements

- Go 1.18+
- ffmpeg + ffprobe on PATH (or `ffmpeg.ffprobe` for snap installs)

### Quick start

```bash
git clone https://github.com/SamirPRO1/video-browser.git
cd video-browser
mkdir -p videos  # put some test videos here
go run main.go --dir ./videos --port 2354
```

Open `http://localhost:2354` — password is `videos123`.

### Rebuild after Go changes

```bash
go build -o video-browser . && ./video-browser --dir ./videos
```

HTML/JS/CSS changes are live immediately (no restart needed).

### Running with a real video library

```bash
go build -o video-browser .
./video-browser --dir /path/to/videos --port 2354 --cache ./thumbcache --clips ./clips
```

---

## 🔌 API Reference

All endpoints require a valid session cookie except `/login`.

### File listing

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/files?path=<rel>` | List directory. Returns `Entry[]`. |
| `DELETE` | `/api/files?path=<rel>` | Delete a file (routes to clipsRoot for CLIPS paths). |

**Entry fields:**
```json
{
  "name": "video.mp4",
  "path": "/folder/video.mp4",
  "vmPath": "/mnt/videos/folder/video.mp4",
  "isDir": false,
  "size": 1234567890,
  "modTime": "2026-05-01T12:00:00Z",
  "hasSprites": true,
  "hasPreview": false,
  "broken": false
}
```

### Sprites

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/sprite/build?path=&width=&height=&cols=&rows=&interval=` | Queue sprite generation for one file |
| `GET` | `/api/sprite/progress?path=` | Progress for one file |
| `POST` | `/api/sprite/build-folder?path=&...` | Queue all videos in a folder |
| `POST/GET` | `/api/sprite/build-all?...` | Build all / get bulk progress |
| `GET` | `/api/sprite/queue` | All sprite jobs |
| `GET` | `/api/sprite?path=&sheet=sprite_001.jpg` | Serve a sprite image |

### Previews

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/preview/build?path=&width=&height=&frames=&quality=` | Queue preview generation |
| `GET` | `/api/preview/progress?path=` | Progress for one file |
| `POST/GET` | `/api/preview/build-all?...` | Build all / get bulk progress |
| `GET` | `/api/preview?path=` | Serve `preview.jpg` |
| `GET` | `/api/preview/meta?path=` | `{"frames":150,"width":320,"height":180}` |

### Unified queue

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/queue` | All sprite + preview jobs merged, sorted by startedAt desc |

### Clip editor

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/video/meta?path=` | Generate (or return cached) sprite sheets + metadata JSON |
| `POST` | `/api/clip` | `{"path":"...","start":10.5,"end":75.2}` → `{"clip":"/folder/CLIPS/video_clip01.mp4"}` |
| `POST` | `/api/recover?path=` | Attempt ffmpeg recovery of broken file |

### File serving

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/files/<path>` | Serve video file. CLIPS paths are transparently routed to `--clips`. |

---

## 🗃️ Cache Structure

```
thumbcache/
└── <rel-path-without-ext>/
    ├── meta.json          # Sprite sheet metadata (duration, interval, cols, rows, sheets[])
    ├── sprite_001.jpg     # Sprite sheet(s)
    ├── sprite_002.jpg
    ├── preview.jpg        # Hover preview strip (Nx frames tiled vertically)
    ├── preview_meta.json  # {"frames":150,"width":320,"height":180}
    └── broken             # Marker file — present if ffprobe failed with moov/invalid error
```

Cache is safe to delete entirely. Everything regenerates on demand.

---

## 🧩 Adding a New Feature

### Adding a new context menu item

1. Add a `<button class="ctx-item">` inside `#ctxMenu` in `index.html`
2. Add a JS function `ctxMyFeature()` that reads `ctxEntry.path` / `ctxEntry.name`
3. If it needs a backend endpoint, add a handler in `main.go` and register it in `main()`

### Adding a new sidebar action

1. Add HTML inside `.sidebar` in `index.html` (use `.sidebar-action` + `.sidebar-action-top` classes)
2. Add the JS function it calls
3. If it has progress, use the existing `startQueuePolling()` pattern

### Adding a new API endpoint

```go
// In main.go — add the handler function:
func myHandler(w http.ResponseWriter, r *http.Request) {
    src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
    if err != nil {
        http.Error(w, "bad path", http.StatusBadRequest)
        return
    }
    // ... your logic
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(result)
}

// In main() — register it:
http.HandleFunc("/api/my-endpoint", requireAuth(myHandler))
```

Always use `resolveInRoot()` to validate paths — it prevents directory traversal.

### Adding a per-file cache artifact

1. Generate and write to `thumbCacheDir(src) + "/yourfile.ext"` 
2. Check existence with `os.Stat` in `listHandler` and add a bool field to `Entry`
3. Add a serving handler that calls `http.ServeFile`

---

## 🐛 Common Issues

| Symptom | Likely cause | Fix |
|---|---|---|
| `ffprobe: exit status 1 — moov atom not found` | Corrupt/incomplete MP4 | Use the Recover button or `untrunc` with a reference file |
| `permission denied` writing to video dir | Directory owned by root | `sudo chown -R $USER:$USER /path/to/videos` |
| Sprites/previews write to wrong place | snap ffmpeg has private `/tmp` | Use paths under `$HOME`; the server already does this for clips and cache |
| `unauthorized` after server restart | Sessions are in-memory | Sign out and back in |
| Preview animation wrong speed | Old `preview_meta.json` missing width/height | Delete `thumbcache/<video>/preview.jpg` and regenerate |
| Queue shows `✗` on preview jobs | ffmpeg tile failed on partial frame count | Fixed in two-step generation — delete old `preview.jpg` and regenerate |

---

## 🚧 Known Limitations & Future Ideas

- **Single password** — no user accounts or per-folder permissions
- **No upload** — read-only by design (clips are the only write path)
- **Sequential bulk generation** — one ffmpeg process at a time; could add concurrency setting
- **In-memory sessions** — lost on restart; could persist to a file or SQLite
- **No search** — would need an index; SQLite + full-text search would fit well
- **Mobile scrubbing in editor** — editor is desktop-only; touch support for In/Out sliders is a future improvement
- **`untrunc` integration** — for severely broken MP4s, automated recovery with a reference file from the same recorder would be powerful
