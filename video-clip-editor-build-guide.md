# Video Clip Editor — Developer Build Guide

A build guide for adding a low-bandwidth, server-side video clip editor on top of the
existing **Go + HTML folder browser**.

---

## 1. Goal & Constraints

We are adding the ability to trim short clips out of videos in the library.

- **The full video never goes to the browser.** The connection to the server is low
  bandwidth, so the editing UI is driven entirely by a small **sprite sheet** of
  thumbnails. The browser never streams or downloads the source video.
- **All cutting happens server-side with ffmpeg**, using stream copy (`-c copy`) — no
  re-encoding. Cuts snap to the nearest keyframe (can be off by a second or two); this
  is acceptable, precision is not required.
- **One clip at a time.** No multi-segment editing, no concatenation, no job queue
  needed — a single ffmpeg subprocess per request is fine.
- **Output clips** are written to the **same folder as the source**, named
  `<source>_clipNN.<ext>` (`holiday.mp4` → `holiday_clip01.mp4`, `holiday_clip02.mp4`, …).
- Typical clips are 2–3 minutes; source videos may be longer.

---

## 2. Architecture / Data Flow

```
Browser (folder browser)
   │  user clicks "Edit" on a video
   ▼
GET /api/video/meta?path=...        ── server generates sprite sheet (once) + reads duration,
   │                                   caches both to disk, returns metadata JSON
   ▼
Editor page loads sprite sheet (small, cached) and renders a timeline.
   │  user drags IN / OUT handles — all scrubbing is sprite-driven, no video downloaded
   ▼
POST /api/clip  { path, start, end }
   │
   ▼
Server runs: ffmpeg -ss <start> -i src -t <dur> -c copy <src>_clipNN.ext
   │
   ▼
Returns the new clip's path → it appears in the folder browser like any other file.
```

Three things cross the wire, all tiny: a metadata JSON, one sprite image (cached by the
browser), and a small JSON of in/out timestamps. The video itself stays on the server.

---

## 3. Prerequisites

The server host needs `ffmpeg` and `ffprobe` on `PATH`.

```bash
ffmpeg -version
ffprobe -version
```

On Debian/Ubuntu: `apt-get install ffmpeg` (includes ffprobe). Verify these exist at
startup and fail loudly if they don't.

---

## 4. Sprite Sheet Generation

Generate **one frame every 2 seconds**, scaled to 160×90, tiled into a 12×12 grid (144
thumbnails per sheet). A 3-minute clip fits in a single sheet the browser caches once.

```bash
ffmpeg -i input.mp4 -vf "fps=1/2,scale=160:90,tile=12x12" -qscale:v 5 sprite_%03d.jpg
```

- `fps=1/2` → the sampling interval (one thumb per 2s). Tune if you want denser scrubbing.
- `tile=12x12` → 144 thumbs per output image. Longer videos produce `sprite_002.jpg`, etc.
- `scale=160:90` assumes 16:9. If sources vary in aspect ratio, use this instead to avoid
  distortion:
  `scale=160:90:force_original_aspect_ratio=decrease,pad=160:90:(ow-iw)/2:(oh-ih)/2`

**Generate lazily and cache.** Generate the sprite the first time a video is opened in the
editor, write it next to a metadata sidecar, and reuse it on every later open. Suggested
cache location: a hidden sibling folder, e.g. `<dir>/.thumbs/<basename>/`.

### Metadata sidecar (write once, alongside the sprites)

```json
{
  "duration": 184.5,
  "interval": 2,
  "cols": 12,
  "rows": 12,
  "thumbWidth": 160,
  "thumbHeight": 90,
  "sheets": ["sprite_001.jpg"]
}
```

Get `duration` from ffprobe:

```bash
ffprobe -v error -show_entries format=duration -of csv=p=0 input.mp4
```

---

## 5. Backend (Go)

Reuse the existing media root and path-resolution logic from the folder browser. Add the
handlers below.

### 5.1 Security: validate every path first

The clip and meta endpoints accept a `path`. **Resolve it against the media root and reject
anything that escapes**, or you have a path-traversal hole.

```go
func resolveInRoot(root, p string) (string, error) {
    full := filepath.Join(root, filepath.Clean("/"+p)) // clean strips ../
    rel, err := filepath.Rel(root, full)
    if err != nil || strings.HasPrefix(rel, "..") {
        return "", fmt.Errorf("path escapes media root")
    }
    return full, nil
}
```

### 5.2 Clip naming helper

```go
func nextClipPath(src string) string {
    dir := filepath.Dir(src)
    ext := filepath.Ext(src)
    base := strings.TrimSuffix(filepath.Base(src), ext)
    re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `_clip(\d+)` + regexp.QuoteMeta(ext) + "$")

    entries, _ := os.ReadDir(dir)
    max := 0
    for _, e := range entries {
        if m := re.FindStringSubmatch(e.Name()); m != nil {
            if n, err := strconv.Atoi(m[1]); err == nil && n > max {
                max = n
            }
        }
    }
    return filepath.Join(dir, fmt.Sprintf("%s_clip%02d%s", base, max+1, ext))
}
```

### 5.3 GET /api/video/meta — sprite + metadata

```go
func metaHandler(w http.ResponseWriter, r *http.Request) {
    src, err := resolveInRoot(mediaRoot, r.URL.Query().Get("path"))
    if err != nil {
        http.Error(w, "bad path", http.StatusBadRequest)
        return
    }

    cacheDir := thumbCacheDir(src) // e.g. <dir>/.thumbs/<basename>/
    metaPath := filepath.Join(cacheDir, "meta.json")

    // Generate once, then serve from cache.
    if _, err := os.Stat(metaPath); os.IsNotExist(err) {
        if err := generateSprites(src, cacheDir); err != nil {
            http.Error(w, "sprite generation failed", http.StatusInternalServerError)
            return
        }
    }

    data, err := os.ReadFile(metaPath)
    if err != nil {
        http.Error(w, "meta read failed", http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    w.Write(data)
}
```

`generateSprites` shells out to ffprobe + ffmpeg (commands in §4), counts how many
`sprite_NNN.jpg` files were produced, and writes `meta.json`. Serve the sprite images
themselves with a static file handler scoped to the cache dir, with a long
`Cache-Control` since they never change.

### 5.4 POST /api/clip — make the cut

```go
type clipRequest struct {
    Path  string  `json:"path"`
    Start float64 `json:"start"` // seconds
    End   float64 `json:"end"`   // seconds
}

func clipHandler(w http.ResponseWriter, r *http.Request) {
    var req clipRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "bad json", http.StatusBadRequest)
        return
    }
    src, err := resolveInRoot(mediaRoot, req.Path)
    if err != nil || req.End <= req.Start {
        http.Error(w, "bad request", http.StatusBadRequest)
        return
    }

    out := nextClipPath(src)
    dur := req.End - req.Start

    // -ss before -i = fast keyframe seek; -t (duration) avoids the -to ref gotcha with -c copy.
    cmd := exec.Command("ffmpeg",
        "-ss", fmt.Sprintf("%.3f", req.Start),
        "-i", src,
        "-t", fmt.Sprintf("%.3f", dur),
        "-c", "copy",
        "-y", out,
    )
    if err := cmd.Run(); err != nil {
        http.Error(w, "ffmpeg failed", http.StatusInternalServerError)
        return
    }

    rel, _ := filepath.Rel(mediaRoot, out)
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]string{"clip": rel})
}
```

> **Why `-ss` before `-i` and `-t` not `-to`:** `-ss` before the input does a fast seek to
> the nearest keyframe. With `-c copy`, `-to` has an ambiguous reference point, while `-t`
> (a plain duration) does not — so compute `dur = end - start` and pass that.

---

## 6. Frontend (HTML + JS Editor Page)

A single page reached from an "Edit" link in the folder browser, e.g.
`/editor?path=<relative-path>`. No video element — the timeline is sprite-driven.

### 6.1 Mapping a time to a sprite cell

```js
function cellFor(t, meta) {
  const idx = Math.floor(t / meta.interval);
  const perSheet = meta.cols * meta.rows;
  const sheet = Math.floor(idx / perSheet);
  const local = idx % perSheet;
  const col = local % meta.cols;
  const row = Math.floor(local / meta.cols);
  return {
    sheet: meta.sheets[Math.min(sheet, meta.sheets.length - 1)],
    x: -col * meta.thumbWidth,
    y: -row * meta.thumbHeight,
  };
}
```

Render the current frame as a `<div>` sized `thumbWidth × thumbHeight` with the sprite as
its `background-image` and `background-position` set to `{x, y}`.

### 6.2 UI requirements

- A **scrubber** (range slider or click-drag track) spanning `0 … meta.duration`. As it
  moves, update the preview div via `cellFor`.
- An **IN** and an **OUT** handle (two sliders, or "Set In" / "Set Out" buttons that
  capture the current scrubber time). Show the selected range and computed duration.
- Optional polish: a filmstrip strip showing several cells across the selection.
- A **Create clip** button that POSTs the request:

```js
async function createClip(path, start, end) {
  const res = await fetch("/api/clip", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ path, start, end }),
  });
  if (!res.ok) throw new Error(await res.text());
  return (await res.json()).clip; // relative path of the new clip
}
```

Show a spinner while the request is in flight; on success, surface the returned clip path
(link back into the folder browser). On error, show the server message.

### 6.3 Page load sequence

1. Read `path` from the query string.
2. `GET /api/video/meta?path=<path>` → metadata JSON (this triggers sprite generation on
   first open; it may take a moment — show a loading state).
3. Preload the sprite image(s) listed in `meta.sheets`.
4. Wire up the scrubber, handles, and Create button.

---

## 7. Edge Cases & Notes

- **Keyframe snapping:** with `-c copy`, the actual cut lands on the nearest keyframe, so
  the output can start/end up to ~1–2s off the handle position. Expected and acceptable.
- **First-open latency:** sprite generation for a long source can take several seconds.
  Generate on first open and cache; subsequent opens are instant.
- **Concurrency:** clips are made one at a time, so no queue. If you later allow parallel
  requests, cap concurrent ffmpeg processes so the host isn't overwhelmed.
- **Cache invalidation:** if a source file is replaced, its `.thumbs` cache is stale.
  Simplest fix: compare source mtime against `meta.json` mtime and regenerate if newer.
- **Disk:** sprites are small but accumulate. A cleanup task that drops `.thumbs` folders
  whose source no longer exists is a nice-to-have.

---

## 8. Suggested Build Order

1. **ffmpeg plumbing** — a small Go function that, given a source path, produces the
   sprite sheet(s) + `meta.json` in a cache dir. Test it from `main` on one file.
2. **`/api/video/meta`** — wire generation behind the endpoint with disk caching.
3. **Editor page (read-only)** — load metadata, preload the sprite, get the scrubber +
   preview cell working. This proves the low-bandwidth scrubbing end to end.
4. **IN/OUT selection** — add the two handles and the duration readout.
5. **`/api/clip` + naming helper** — server-side cut, write `<src>_clipNN.ext`.
6. **Wire the button** — POST, spinner, success/error, link to the new clip.
7. **Hardening** — path validation, ffmpeg-missing checks, cache invalidation.

Steps 1–3 are the riskiest part (they prove the core idea); everything after is
straightforward.
