package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// --- Child process tracker ---
// Keeps a reference to every ffmpeg subprocess so they can be killed on shutdown.
var (
	childProcs   = map[int]*os.Process{}
	childProcsMu sync.Mutex
)

func trackCmd(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	childProcsMu.Lock()
	childProcs[cmd.Process.Pid] = cmd.Process
	childProcsMu.Unlock()
}

func untrackCmd(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	childProcsMu.Lock()
	delete(childProcs, cmd.Process.Pid)
	childProcsMu.Unlock()
}

func killAllChildren() {
	childProcsMu.Lock()
	defer childProcsMu.Unlock()
	for pid, proc := range childProcs {
		log.Printf("killing child ffmpeg pid %d", pid)
		proc.Kill()
	}
}

// --- Hardcoded password ---
const hardcodedPassword = "videos123"

var (
	port      = flag.Int("port", 2354, "Port to listen on")
	videoRoot = flag.String("dir", "./videos", "Root folder with video files")
	cacheRoot = flag.String("cache", "./thumbcache", "Directory for thumbnail sprite cache")
	clipsRoot = flag.String("clips", "./clips", "Directory where clips are saved")
)

// clipsSentinel is the virtual folder name shown in the browser.
const clipsSentinel = "CLIPS"

// splitAtClips detects a CLIPS sentinel in the path.
// "/foo/CLIPS"         → realDir="/foo", rest="",        isClips=true
// "/foo/CLIPS/f.mp4"  → realDir="/foo", rest="f.mp4",   isClips=true
// "/foo/bar"           → "", "", false
func splitAtClips(p string) (realDir, rest string, isClips bool) {
	clean := filepath.ToSlash(filepath.Clean("/" + p))
	parts := strings.Split(clean, "/") // parts[0] == ""
	for i, part := range parts {
		if part == clipsSentinel {
			dir := strings.Join(parts[:i], "/")
			if dir == "" {
				dir = "/"
			}
			return dir, strings.Join(parts[i+1:], "/"), true
		}
	}
	return "", "", false
}

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true,
	".mov": true, ".avi": true, ".m4v": true,
	".ts": true, ".ogv": true,
}

// ffmpeg/ffprobe binaries (resolved at startup)
var (
	ffmpegBin  string
	ffprobeBin string
)

func findBin(names ...string) string {
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// --- Session store (persisted to disk) ---
var (
	sessions     = map[string]time.Time{}
	sessionsMu   sync.Mutex
	sessionsPath = "./sessions.json"
)

func saveSessions() {
	sessionsMu.Lock()
	// Snapshot non-expired sessions
	live := make(map[string]time.Time, len(sessions))
	now := time.Now()
	for tok, exp := range sessions {
		if exp.After(now) {
			live[tok] = exp
		}
	}
	sessionsMu.Unlock()
	data, _ := json.Marshal(live)
	tmp := sessionsPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		os.Rename(tmp, sessionsPath)
	}
}

func loadSessions() {
	data, err := os.ReadFile(sessionsPath)
	if err != nil {
		return
	}
	var loaded map[string]time.Time
	if err := json.Unmarshal(data, &loaded); err != nil {
		return
	}
	sessionsMu.Lock()
	now := time.Now()
	for tok, exp := range loaded {
		if exp.After(now) {
			sessions[tok] = exp
		}
	}
	sessionsMu.Unlock()
	log.Printf("Loaded %d existing session(s) from disk", len(sessions))
}

func newSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	sessionsMu.Lock()
	sessions[token] = time.Now().Add(24 * time.Hour)
	sessionsMu.Unlock()
	saveSessions()
	return token
}

func validSession(token string) bool {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	exp, ok := sessions[token]
	return ok && time.Now().Before(exp)
}

func deleteSession(token string) {
	sessionsMu.Lock()
	delete(sessions, token)
	sessionsMu.Unlock()
	saveSessions()
}

// --- Auth middleware ---
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || !validSession(cookie.Value) {
			if r.Header.Get("Accept") == "application/json" ||
				strings.HasPrefix(r.URL.Path, "/api/") ||
				strings.HasPrefix(r.URL.Path, "/files/") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// --- Login handlers ---
func loginPageHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil && validSession(c.Value) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.ServeFile(w, r, "login.html")
}

func loginPostHandler(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	pw := r.FormValue("password")
	if pw != hardcodedPassword {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	token := newSession()
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("session"); err == nil {
		deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:    "session",
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
		MaxAge:  -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- File listing ---
type Entry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	VMPath     string `json:"vmPath,omitempty"`
	IsDir      bool   `json:"isDir"`
	Size       int64  `json:"size,omitempty"`
	ModTime    string `json:"modTime,omitempty"`
	HasSprites bool   `json:"hasSprites,omitempty"`
	HasPreview bool   `json:"hasPreview,omitempty"`
	Broken     bool   `json:"broken,omitempty"`
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	w.Header().Set("Content-Type", "application/json")

	// Virtual CLIPS folder
	if realDir, _, isClips := splitAtClips(raw); isClips {
		clipsDir := filepath.Join(*clipsRoot, filepath.Clean("/"+realDir))
		if !strings.HasPrefix(clipsDir, filepath.Clean(*clipsRoot)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		entries, err := os.ReadDir(clipsDir)
		if err != nil {
			json.NewEncoder(w).Encode([]Entry{})
			return
		}
		var result []Entry
		for _, e := range entries {
			if e.IsDir() || !videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
				continue
			}
			info, _ := e.Info()
			absPath := filepath.Join(clipsDir, e.Name())
			virtPath := filepath.ToSlash(filepath.Join(realDir, clipsSentinel, e.Name()))
			entry := Entry{Name: e.Name(), Path: virtPath, VMPath: absPath}
			if info != nil {
				entry.Size = info.Size()
				entry.ModTime = info.ModTime().Format(time.RFC3339)
			}
			result = append(result, entry)
		}
		sort.Slice(result, func(i, j int) bool {
			return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
		})
		json.NewEncoder(w).Encode(result)
		return
	}

	// Normal directory
	rel := filepath.Clean("/" + raw)
	abs := filepath.Join(*videoRoot, rel)
	if !strings.HasPrefix(abs, filepath.Clean(*videoRoot)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var result []Entry
	for _, e := range entries {
		info, _ := e.Info()
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.IsDir() && !videoExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		entryPath := filepath.ToSlash(filepath.Join(rel, name))
		absPath := filepath.Join(abs, name)
		entry := Entry{Name: name, Path: entryPath, VMPath: absPath, IsDir: e.IsDir()}
		if info != nil {
			entry.ModTime = info.ModTime().Format(time.RFC3339)
			if e.IsDir() {
				entry.Size = dirSize(absPath)
			} else {
				entry.Size = info.Size()
			}
		}
		if !e.IsDir() {
			cacheDir := thumbCacheDir(absPath)
			if _, err := os.Stat(filepath.Join(cacheDir, "meta.json")); err == nil {
				entry.HasSprites = true
			}
			if _, err := os.Stat(filepath.Join(cacheDir, "preview.jpg")); err == nil {
				entry.HasPreview = true
			}
			if _, err := os.Stat(filepath.Join(cacheDir, "broken")); err == nil {
				entry.Broken = true
			}
		}
		result = append(result, entry)
	}

	// Inject virtual CLIPS folder if clips exist for this directory
	clipsDir := filepath.Join(*clipsRoot, rel)
	if dirInfo, err := os.Stat(clipsDir); err == nil && dirInfo.IsDir() {
		clipsEntries, _ := os.ReadDir(clipsDir)
		for _, e := range clipsEntries {
			if !e.IsDir() && videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
				result = append(result, Entry{
					Name:  clipsSentinel,
					Path:  filepath.ToSlash(filepath.Join(rel, clipsSentinel)),
					IsDir: true,
				})
				break
			}
		}
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})

	json.NewEncoder(w).Encode(result)
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")

	var abs string
	if realDir, filename, isClips := splitAtClips(raw); isClips {
		// Clip stored in clipsRoot
		abs = filepath.Join(*clipsRoot, filepath.Clean("/"+realDir), filename)
		if !strings.HasPrefix(abs, filepath.Clean(*clipsRoot)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	} else {
		rel := filepath.Clean("/" + raw)
		abs = filepath.Join(*videoRoot, rel)
		if !strings.HasPrefix(abs, filepath.Clean(*videoRoot)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot delete directories", http.StatusBadRequest)
		return
	}

	if err := os.Remove(abs); err != nil {
		http.Error(w, "failed to delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	invalidateDirSizeCache(filepath.Dir(abs))
	w.WriteHeader(http.StatusNoContent)
}

// --- Clip editor helpers ---

func resolveInRoot(root, p string) (string, error) {
	full := filepath.Join(root, filepath.Clean("/"+p))
	rel, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes media root")
	}
	return full, nil
}

var dirSizeCache sync.Map // abs path → int64

func dirSize(path string) int64 {
	if v, ok := dirSizeCache.Load(path); ok {
		return v.(int64)
	}
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	dirSizeCache.Store(path, total)
	return total
}

func invalidateDirSizeCache(path string) {
	// Walk up and invalidate every ancestor inside videoRoot.
	for p := path; strings.HasPrefix(p, filepath.Clean(*videoRoot)); p = filepath.Dir(p) {
		dirSizeCache.Delete(p)
	}
}

func thumbCacheDir(src string) string {
	rel, err := filepath.Rel(*videoRoot, src)
	if err != nil {
		rel = filepath.Base(src)
	}
	base := strings.TrimSuffix(rel, filepath.Ext(rel))
	return filepath.Join(*cacheRoot, base)
}

type SpriteMeta struct {
	Duration    float64  `json:"duration"`
	Interval    int      `json:"interval"`
	Cols        int      `json:"cols"`
	Rows        int      `json:"rows"`
	ThumbWidth  int      `json:"thumbWidth"`
	ThumbHeight int      `json:"thumbHeight"`
	Sheets      []string `json:"sheets"`
}

// --- Sprite config ---

type spriteConfig struct {
	Interval int `json:"interval"` // seconds between frames
	Cols     int `json:"cols"`
	Rows     int `json:"rows"`
	Width    int `json:"width"`
	Height   int `json:"height"`
}

func defaultSpriteConfig() spriteConfig {
	return spriteConfig{Interval: 2, Cols: 12, Rows: 12, Width: 160, Height: 90}
}

func spriteConfigFromQuery(q interface{ Get(string) string }) spriteConfig {
	cfg := defaultSpriteConfig()
	if v, err := strconv.Atoi(q.Get("interval")); err == nil && v > 0 {
		cfg.Interval = v
	}
	if v, err := strconv.Atoi(q.Get("cols")); err == nil && v > 0 {
		cfg.Cols = v
	}
	if v, err := strconv.Atoi(q.Get("rows")); err == nil && v > 0 {
		cfg.Rows = v
	}
	if v, err := strconv.Atoi(q.Get("width")); err == nil && v > 0 {
		cfg.Width = v
	}
	if v, err := strconv.Atoi(q.Get("height")); err == nil && v > 0 {
		cfg.Height = v
	}
	return cfg
}

func spriteVF(cfg spriteConfig) string {
	return fmt.Sprintf(
		"fps=1/%d,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,tile=%dx%d",
		cfg.Interval, cfg.Width, cfg.Height, cfg.Width, cfg.Height, cfg.Cols, cfg.Rows,
	)
}

var brokenIndicators = []string{
	"moov atom not found",
	"Invalid data found when processing input",
	"no such file",
	"Permission denied",
}

func writeBrokenMarker(src, msg string) {
	cacheDir := thumbCacheDir(src)
	os.MkdirAll(cacheDir, 0755)
	os.WriteFile(filepath.Join(cacheDir, "broken"), []byte(msg), 0644)
}

func clearBrokenMarker(src string) {
	os.Remove(filepath.Join(thumbCacheDir(src), "broken"))
}

func isBroken(absPath string) bool {
	_, err := os.Stat(filepath.Join(thumbCacheDir(absPath), "broken"))
	return err == nil
}

func probeDuration(src string) (float64, error) {
	cmd := exec.Command(ffprobeBin,
		"-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", src,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		for _, ind := range brokenIndicators {
			if strings.Contains(strings.ToLower(msg), strings.ToLower(ind)) {
				writeBrokenMarker(src, msg)
				break
			}
		}
		return 0, fmt.Errorf("ffprobe: %w — %s", err, msg)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if d, err := strconv.ParseFloat(line, 64); err == nil && d > 0 {
			return d, nil
		}
	}
	return 0, fmt.Errorf("no duration in ffprobe output: %q", strings.TrimSpace(string(out)))
}

func collectSpriteSheets(cacheDir string) []string {
	entries, _ := os.ReadDir(cacheDir)
	var sheets []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sprite_") && strings.HasSuffix(e.Name(), ".jpg") {
			sheets = append(sheets, e.Name())
		}
	}
	sort.Strings(sheets)
	return sheets
}

func generateSprites(src, cacheDir string, cfg spriteConfig) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	duration, err := probeDuration(src)
	if err != nil {
		return err
	}
	spritePat := filepath.Join(cacheDir, "sprite_%03d.jpg")
	cmd := exec.Command(ffmpegBin,
		"-skip_frame", "nointra",
		"-i", src,
		"-vf", spriteVF(cfg),
		"-vsync", "vfr",
		"-qscale:v", "5",
		"-y", spritePat,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg sprites: %w\n%s", err, out)
	}
	sheets := collectSpriteSheets(cacheDir)
	if len(sheets) == 0 {
		return fmt.Errorf("no sprite sheets produced")
	}
	meta := SpriteMeta{Duration: duration, Interval: cfg.Interval, Cols: cfg.Cols, Rows: cfg.Rows,
		ThumbWidth: cfg.Width, ThumbHeight: cfg.Height, Sheets: sheets}
	data, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(cacheDir, "meta.json"), data, 0644)
}

// --- Sprite build job tracking ---

type spriteJob struct {
	Path      string       `json:"path"`
	Name      string       `json:"name"`
	Percent   int          `json:"percent"`
	Done      bool         `json:"done"`
	Err       string       `json:"err,omitempty"`
	StartedAt time.Time    `json:"startedAt"`
	Cfg       spriteConfig `json:"config"`
}

var (
	spriteJobs   = map[string]*spriteJob{}
	spriteJobsMu sync.Mutex
)

func getOrCreateSpriteJob(relPath, name string, cfg spriteConfig) (*spriteJob, bool) {
	spriteJobsMu.Lock()
	defer spriteJobsMu.Unlock()
	if j, ok := spriteJobs[relPath]; ok && !j.Done {
		return j, false
	}
	j := &spriteJob{Path: relPath, Name: name, StartedAt: time.Now(), Cfg: cfg}
	spriteJobs[relPath] = j
	return j, true
}

func generateSpritesAsync(relPath, src string, job *spriteJob) {
	cfg := job.Cfg
	cacheDir := thumbCacheDir(src)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, err.Error()
		spriteJobsMu.Unlock()
		return
	}

	duration, err := probeDuration(src)
	if err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, err.Error()
		spriteJobsMu.Unlock()
		return
	}

	spritePat := filepath.Join(cacheDir, "sprite_%03d.jpg")
	cmd := exec.Command(ffmpegBin,
		"-skip_frame", "nointra",
		"-progress", "pipe:1", "-nostats",
		"-i", src,
		"-vf", spriteVF(cfg),
		"-vsync", "vfr",
		"-qscale:v", "5",
		"-y", spritePat,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, err.Error()
		spriteJobsMu.Unlock()
		return
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, err.Error()
		spriteJobsMu.Unlock()
		return
	}
	trackCmd(cmd)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_ms=") {
			if ms, err := strconv.ParseInt(strings.TrimPrefix(line, "out_time_ms="), 10, 64); err == nil && ms > 0 {
				pct := int(float64(ms) / 1e6 / duration * 100)
				if pct > 99 {
					pct = 99
				}
				spriteJobsMu.Lock()
				job.Percent = pct
				spriteJobsMu.Unlock()
			}
		}
	}

	untrackCmd(cmd)
	if err := cmd.Wait(); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, "ffmpeg: "+err.Error()
		spriteJobsMu.Unlock()
		return
	}

	sheets := collectSpriteSheets(cacheDir)
	meta := SpriteMeta{Duration: duration, Interval: cfg.Interval, Cols: cfg.Cols, Rows: cfg.Rows,
		ThumbWidth: cfg.Width, ThumbHeight: cfg.Height, Sheets: sheets}
	data, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(cacheDir, "meta.json"), data, 0644); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, "write meta: "+err.Error()
		spriteJobsMu.Unlock()
		return
	}
	spriteJobsMu.Lock()
	job.Percent = 100
	job.Done = true
	spriteJobsMu.Unlock()
}

func spriteBuildHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := r.URL.Query().Get("path")
	src, err := resolveInRoot(*videoRoot, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	cfg := spriteConfigFromQuery(r.URL.Query())
	name := filepath.Base(src)
	job, created := getOrCreateSpriteJob(relPath, name, cfg)
	if created {
		go generateSpritesAsync(relPath, src, job)
	}
	w.Header().Set("Content-Type", "application/json")
	spriteJobsMu.Lock()
	json.NewEncoder(w).Encode(job)
	spriteJobsMu.Unlock()
}

func spriteProgressHandler(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	w.Header().Set("Content-Type", "application/json")
	spriteJobsMu.Lock()
	if job, ok := spriteJobs[relPath]; ok {
		json.NewEncoder(w).Encode(job)
		spriteJobsMu.Unlock()
		return
	}
	spriteJobsMu.Unlock()
	src, err := resolveInRoot(*videoRoot, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filepath.Join(thumbCacheDir(src), "meta.json")); err == nil {
		json.NewEncoder(w).Encode(&spriteJob{Percent: 100, Done: true})
	} else {
		json.NewEncoder(w).Encode(&spriteJob{})
	}
}

func spriteQueueHandler(w http.ResponseWriter, r *http.Request) {
	spriteJobsMu.Lock()
	jobs := make([]*spriteJob, 0, len(spriteJobs))
	for _, j := range spriteJobs {
		jobs = append(jobs, j)
	}
	spriteJobsMu.Unlock()
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartedAt.After(jobs[j].StartedAt)
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jobs)
}

// --- Unified queue ---

type queueItem struct {
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Type      string    `json:"type"` // "sprite" or "preview"
	Percent   int       `json:"percent"`
	Done      bool      `json:"done"`
	Err       string    `json:"err,omitempty"`
	StartedAt time.Time `json:"startedAt"`
}

func unifiedQueueHandler(w http.ResponseWriter, r *http.Request) {
	var items []queueItem

	spriteJobsMu.Lock()
	for _, j := range spriteJobs {
		items = append(items, queueItem{
			Path: j.Path, Name: j.Name, Type: "sprite",
			Percent: j.Percent, Done: j.Done, Err: j.Err, StartedAt: j.StartedAt,
		})
	}
	spriteJobsMu.Unlock()

	previewJobsMu.Lock()
	for _, j := range previewJobs {
		items = append(items, queueItem{
			Path: j.Path, Name: j.Name, Type: "preview",
			Percent: j.Percent, Done: j.Done, Err: j.Err, StartedAt: j.StartedAt,
		})
	}
	previewJobsMu.Unlock()

	sort.Slice(items, func(i, j int) bool {
		return items[i].StartedAt.After(items[j].StartedAt)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// spriteBuildFolderHandler queues sprite generation for all videos in one folder (non-recursive).
func spriteBuildFolderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw := r.URL.Query().Get("path")
	rel := filepath.Clean("/" + raw)
	abs := filepath.Join(*videoRoot, rel)
	if !strings.HasPrefix(abs, filepath.Clean(*videoRoot)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cfg := spriteConfigFromQuery(r.URL.Query())
	queued := 0
	for _, e := range entries {
		if e.IsDir() || !videoExts[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		src := filepath.Join(abs, e.Name())
		relPath := filepath.ToSlash(filepath.Join(rel, e.Name()))
		job, created := getOrCreateSpriteJob(relPath, e.Name(), cfg)
		if created {
			go generateSpritesAsync(relPath, src, job)
			queued++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"queued": queued})
}

// --- Bulk sprite generation (all videos) ---

type bulkSpriteJob struct {
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Current string `json:"current,omitempty"`
	Running bool   `json:"running"`
}

var (
	bulkSprite   bulkSpriteJob
	bulkSpriteMu sync.Mutex
)

func runBulkSprite(cfg spriteConfig) {
	videos, err := collectVideos(*videoRoot)
	if err != nil {
		bulkSpriteMu.Lock()
		bulkSprite.Running = false
		bulkSpriteMu.Unlock()
		return
	}
	var pending []string
	for _, v := range videos {
		if _, err := os.Stat(filepath.Join(thumbCacheDir(v), "meta.json")); os.IsNotExist(err) {
			pending = append(pending, v)
		}
	}
	bulkSpriteMu.Lock()
	bulkSprite.Total = len(pending)
	bulkSprite.Done = 0
	bulkSpriteMu.Unlock()

	for _, src := range pending {
		rel, _ := filepath.Rel(*videoRoot, src)
		relPath := "/" + filepath.ToSlash(rel)
		name := filepath.Base(src)

		bulkSpriteMu.Lock()
		bulkSprite.Current = name
		bulkSpriteMu.Unlock()

		job, created := getOrCreateSpriteJob(relPath, name, cfg)
		if created {
			generateSpritesAsync(relPath, src, job) // blocking — one at a time
		} else {
			for {
				spriteJobsMu.Lock()
				done := job.Done
				spriteJobsMu.Unlock()
				if done {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
		bulkSpriteMu.Lock()
		bulkSprite.Done++
		bulkSpriteMu.Unlock()
	}
	bulkSpriteMu.Lock()
	bulkSprite.Running = false
	bulkSprite.Current = ""
	bulkSpriteMu.Unlock()
}

func spriteBuildAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bulkSpriteMu.Lock()
	if bulkSprite.Running {
		bulkSpriteMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bulkSprite)
		return
	}
	bulkSprite = bulkSpriteJob{Running: true}
	cfg := spriteConfigFromQuery(r.URL.Query())
	bulkSpriteMu.Unlock()
	go runBulkSprite(cfg)
	w.Header().Set("Content-Type", "application/json")
	bulkSpriteMu.Lock()
	json.NewEncoder(w).Encode(bulkSprite)
	bulkSpriteMu.Unlock()
}

func spriteBuildAllProgressHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	bulkSpriteMu.Lock()
	json.NewEncoder(w).Encode(bulkSprite)
	bulkSpriteMu.Unlock()
}

// --- Preview (hover strip) ---

type previewConfig struct {
	Frames  int `json:"frames"`  // number of frames in the strip
	Width   int `json:"width"`   // frame width px
	Height  int `json:"height"`  // frame height px
	Quality int `json:"quality"` // ffmpeg -q:v (1=best, 10=worst)
}

func defaultPreviewConfig() previewConfig {
	return previewConfig{Frames: 150, Width: 320, Height: 180, Quality: 4}
}

func previewConfigFromQuery(q interface{ Get(string) string }) previewConfig {
	cfg := defaultPreviewConfig()
	if v, err := strconv.Atoi(q.Get("frames")); err == nil && v > 0 {
		cfg.Frames = v
	}
	if v, err := strconv.Atoi(q.Get("width")); err == nil && v > 0 {
		cfg.Width = v
	}
	if v, err := strconv.Atoi(q.Get("height")); err == nil && v > 0 {
		cfg.Height = v
	}
	if v, err := strconv.Atoi(q.Get("quality")); err == nil && v > 0 {
		cfg.Quality = v
	}
	return cfg
}

type previewJob struct {
	Path      string        `json:"path"`
	Name      string        `json:"name"`
	Percent   int           `json:"percent"`
	Done      bool          `json:"done"`
	Err       string        `json:"err,omitempty"`
	StartedAt time.Time     `json:"startedAt"`
	Cfg       previewConfig `json:"config"`
}

var (
	previewJobs   = map[string]*previewJob{}
	previewJobsMu sync.Mutex
)

func getOrCreatePreviewJob(relPath string, cfg previewConfig) (*previewJob, bool) {
	previewJobsMu.Lock()
	defer previewJobsMu.Unlock()
	if j, ok := previewJobs[relPath]; ok && !j.Done {
		return j, false
	}
	j := &previewJob{Path: relPath, Name: filepath.Base(relPath), StartedAt: time.Now(), Cfg: cfg}
	previewJobs[relPath] = j
	return j, true
}

func generatePreviewAsync(relPath, src string, job *previewJob) {
	setErr := func(msg string) {
		previewJobsMu.Lock()
		job.Done, job.Err = true, msg
		previewJobsMu.Unlock()
	}

	cacheDir := thumbCacheDir(src)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		setErr(err.Error())
		return
	}

	duration, err := probeDuration(src)
	if err != nil {
		setErr(err.Error())
		return
	}

	cfg := job.Cfg

	// Clean up any leftover frame files from a previous interrupted run.
	old, _ := filepath.Glob(filepath.Join(cacheDir, "pframe_*.jpg"))
	for _, f := range old {
		os.Remove(f)
	}

	// Step 1 — extract frames individually.
	// Each frame is written as it's decoded, so out_time_ms advances
	// continuously and gives real progress throughout the video.
	fpsExpr := fmt.Sprintf("%d/%d", cfg.Frames, int(math.Ceil(duration)))
	framePat := filepath.Join(cacheDir, "pframe_%03d.jpg")
	vf := fmt.Sprintf(
		"fps=%s,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
		fpsExpr, cfg.Width, cfg.Height, cfg.Width, cfg.Height,
	)
	cmd1 := exec.Command(ffmpegBin,
		"-skip_frame", "nointra", // decode keyframes only — 60-150x faster than full decode
		"-progress", "pipe:1", "-nostats",
		"-i", src,
		"-vf", vf,
		"-vsync", "vfr", // variable timestamps produced by skip_frame
		"-q:v", strconv.Itoa(cfg.Quality),
		"-y", framePat,
	)
	stdout, err := cmd1.StdoutPipe()
	if err != nil {
		setErr(err.Error())
		return
	}
	cmd1.Stderr = io.Discard
	if err := cmd1.Start(); err != nil {
		setErr(err.Error())
		return
	}
	trackCmd(cmd1)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_ms=") {
			if ms, err := strconv.ParseInt(strings.TrimPrefix(line, "out_time_ms="), 10, 64); err == nil && ms > 0 {
				pct := int(float64(ms) / 1e6 / duration * 95) // reserve 95-100 for step 2
				if pct > 95 {
					pct = 95
				}
				previewJobsMu.Lock()
				job.Percent = pct
				previewJobsMu.Unlock()
			}
		}
	}

	untrackCmd(cmd1)
	if err := cmd1.Wait(); err != nil {
		setErr("ffmpeg (frames): " + err.Error())
		return
	}

	// Count actual frames produced — tile uses this so it never fails
	// due to rounding (short videos, unusual framerates, etc.)
	frameFiles, _ := filepath.Glob(filepath.Join(cacheDir, "pframe_*.jpg"))
	actualFrames := len(frameFiles)
	if actualFrames == 0 {
		setErr("no frames produced")
		return
	}

	// Step 2 — tile frames into single vertical strip. Nearly instant.
	previewPath := filepath.Join(cacheDir, "preview.jpg")
	cmd2 := exec.Command(ffmpegBin,
		"-i", framePat,
		"-vf", fmt.Sprintf("tile=1x%d", actualFrames),
		"-frames:v", "1",
		"-q:v", strconv.Itoa(cfg.Quality),
		"-y", previewPath,
	)
	if out, err := cmd2.CombinedOutput(); err != nil {
		for _, f := range frameFiles {
			os.Remove(f)
		}
		setErr("ffmpeg (tile): " + string(out))
		return
	}

	for _, f := range frameFiles {
		os.Remove(f)
	}

	type previewMeta struct {
		Frames  int `json:"frames"`
		Width   int `json:"width"`
		Height  int `json:"height"`
	}
	metaData, _ := json.Marshal(previewMeta{Frames: actualFrames, Width: cfg.Width, Height: cfg.Height})
	os.WriteFile(filepath.Join(cacheDir, "preview_meta.json"), metaData, 0644)

	previewJobsMu.Lock()
	job.Percent = 100
	job.Done = true
	previewJobsMu.Unlock()
}

func previewBuildHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := r.URL.Query().Get("path")
	src, err := resolveInRoot(*videoRoot, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	// If the file already exists on disk, skip — the in-memory job map may
	// have expired (Done=true) after a restart or a completed Generate All,
	// causing stale-listing hovers to re-trigger generation unnecessarily.
	previewPath := filepath.Join(thumbCacheDir(src), "preview.jpg")
	if _, err := os.Stat(previewPath); err == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(&previewJob{Path: relPath, Name: filepath.Base(src), Percent: 100, Done: true})
		return
	}

	cfg := previewConfigFromQuery(r.URL.Query())
	job, created := getOrCreatePreviewJob(relPath, cfg)
	if created {
		go generatePreviewAsync(relPath, src, job)
	}
	w.Header().Set("Content-Type", "application/json")
	previewJobsMu.Lock()
	json.NewEncoder(w).Encode(job)
	previewJobsMu.Unlock()
}

func previewProgressHandler(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("path")
	w.Header().Set("Content-Type", "application/json")

	previewJobsMu.Lock()
	if job, ok := previewJobs[relPath]; ok {
		json.NewEncoder(w).Encode(job)
		previewJobsMu.Unlock()
		return
	}
	previewJobsMu.Unlock()

	src, err := resolveInRoot(*videoRoot, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	previewPath := filepath.Join(thumbCacheDir(src), "preview.jpg")
	if _, err := os.Stat(previewPath); err == nil {
		json.NewEncoder(w).Encode(&previewJob{Percent: 100, Done: true})
	} else {
		json.NewEncoder(w).Encode(&previewJob{})
	}
}

func previewImageHandler(w http.ResponseWriter, r *http.Request) {
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	imgPath := filepath.Join(thumbCacheDir(src), "preview.jpg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, imgPath)
}

func previewMetaHandler(w http.ResponseWriter, r *http.Request) {
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	metaPath := filepath.Join(thumbCacheDir(src), "preview_meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		cfg := defaultPreviewConfig()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"frames":%d,"width":%d,"height":%d}`, cfg.Frames, cfg.Width, cfg.Height)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

// --- Bulk preview generation ---

type bulkJob struct {
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Current string `json:"current,omitempty"`
	Running bool   `json:"running"`
}

var (
	bulk   bulkJob
	bulkMu sync.Mutex
)

func collectVideos(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		if videoExts[strings.ToLower(filepath.Ext(p))] {
			files = append(files, p)
		}
		return nil
	})
	return files, err
}

// --- Search ---

func searchHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	w.Header().Set("Content-Type", "application/json")
	if q == "" {
		json.NewEncoder(w).Encode([]Entry{})
		return
	}

	// Cap results so a huge library doesn't lock up the browser
	const maxResults = 200
	var results []Entry

	rootClean := filepath.Clean(*videoRoot)
	errStop := fmt.Errorf("stop")
	filepath.Walk(*videoRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !videoExts[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		if !strings.Contains(strings.ToLower(name), q) {
			return nil
		}
		rel, err := filepath.Rel(rootClean, p)
		if err != nil {
			return nil
		}
		cacheDir := thumbCacheDir(p)
		entry := Entry{
			Name:    name,
			Path:    "/" + filepath.ToSlash(rel),
			VMPath:  p,
			Size:    info.Size(),
			ModTime: info.ModTime().Format(time.RFC3339),
		}
		if _, err := os.Stat(filepath.Join(cacheDir, "meta.json")); err == nil {
			entry.HasSprites = true
		}
		if _, err := os.Stat(filepath.Join(cacheDir, "preview.jpg")); err == nil {
			entry.HasPreview = true
		}
		if _, err := os.Stat(filepath.Join(cacheDir, "broken")); err == nil {
			entry.Broken = true
		}
		results = append(results, entry)
		if len(results) >= maxResults {
			return errStop
		}
		return nil
	})

	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Name) < strings.ToLower(results[j].Name)
	})

	json.NewEncoder(w).Encode(results)
}

// --- Video info ---

type videoInfo struct {
	Path     string  `json:"path"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
	Codec    string  `json:"codec"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	FPS      string  `json:"fps"`
	Bitrate  int64   `json:"bitrate"`
	Format   string  `json:"format"`
	Audio    string  `json:"audio"`
}

func infoHandler(w http.ResponseWriter, r *http.Request) {
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		// Try CLIPS path
		if realDir, filename, isClips := splitAtClips(r.URL.Query().Get("path")); isClips {
			src = filepath.Join(*clipsRoot, filepath.Clean("/"+realDir), filename)
			if !strings.HasPrefix(src, filepath.Clean(*clipsRoot)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		} else {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
	}

	stat, err := os.Stat(src)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	cmd := exec.Command(ffprobeBin,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		src,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		http.Error(w, "ffprobe failed: "+string(out), http.StatusInternalServerError)
		return
	}

	var probe struct {
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
			BitRate    string `json:"bit_rate"`
		} `json:"format"`
		Streams []struct {
			CodecType    string `json:"codec_type"`
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			RFrameRate   string `json:"r_frame_rate"`
			Channels     int    `json:"channels"`
			SampleRate   string `json:"sample_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		http.Error(w, "ffprobe parse: "+err.Error(), http.StatusInternalServerError)
		return
	}

	info := videoInfo{
		Path:   r.URL.Query().Get("path"),
		Size:   stat.Size(),
		Format: probe.Format.FormatName,
	}
	if d, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
		info.Duration = d
	}
	if br, err := strconv.ParseInt(probe.Format.BitRate, 10, 64); err == nil {
		info.Bitrate = br
	}
	for _, s := range probe.Streams {
		switch s.CodecType {
		case "video":
			if info.Codec == "" {
				info.Codec = s.CodecName
				info.Width = s.Width
				info.Height = s.Height
				// r_frame_rate is something like "30/1" — simplify it
				if parts := strings.Split(s.RFrameRate, "/"); len(parts) == 2 {
					num, _ := strconv.ParseFloat(parts[0], 64)
					den, _ := strconv.ParseFloat(parts[1], 64)
					if den > 0 {
						info.FPS = fmt.Sprintf("%.2f", num/den)
					}
				}
			}
		case "audio":
			if info.Audio == "" {
				info.Audio = fmt.Sprintf("%s, %dch", s.CodecName, s.Channels)
				if s.SampleRate != "" {
					info.Audio += ", " + s.SampleRate + " Hz"
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func runBulkPreview(cfg previewConfig) {
	videos, err := collectVideos(*videoRoot)
	if err != nil {
		bulkMu.Lock()
		bulk.Running = false
		bulkMu.Unlock()
		return
	}

	// Filter to only those without a preview
	var pending []string
	for _, v := range videos {
		previewPath := filepath.Join(thumbCacheDir(v), "preview.jpg")
		if _, err := os.Stat(previewPath); os.IsNotExist(err) {
			pending = append(pending, v)
		}
	}

	bulkMu.Lock()
	bulk.Total = len(pending)
	bulk.Done = 0
	bulkMu.Unlock()

	for _, src := range pending {
		rel, _ := filepath.Rel(*videoRoot, src)
		relPath := "/" + filepath.ToSlash(rel)

		bulkMu.Lock()
		bulk.Current = filepath.Base(src)
		bulkMu.Unlock()

		job, created := getOrCreatePreviewJob(relPath, cfg)
		if created {
			generatePreviewAsync(relPath, src, job) // blocking — one at a time
		} else {
			// Wait for existing job to finish
			for {
				previewJobsMu.Lock()
				done := job.Done
				previewJobsMu.Unlock()
				if done {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
		}

		bulkMu.Lock()
		bulk.Done++
		bulkMu.Unlock()
	}

	bulkMu.Lock()
	bulk.Running = false
	bulk.Current = ""
	bulkMu.Unlock()
}

func previewBuildAllHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	bulkMu.Lock()
	if bulk.Running {
		bulkMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bulk)
		return
	}
	bulk = bulkJob{Running: true}
	cfg := previewConfigFromQuery(r.URL.Query())
	bulkMu.Unlock()

	go runBulkPreview(cfg)

	w.Header().Set("Content-Type", "application/json")
	bulkMu.Lock()
	json.NewEncoder(w).Encode(bulk)
	bulkMu.Unlock()
}

func previewBuildAllProgressHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	bulkMu.Lock()
	json.NewEncoder(w).Encode(bulk)
	bulkMu.Unlock()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._\- ]+`)

// sanitizeClipName returns a filesystem-safe name without extension, or "" if invalid.
func sanitizeClipName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Strip extension if user typed one — we always reuse the source extension.
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = unsafeName.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// nextClipPath returns the physical save path and the virtual browser path for the next clip.
// If customName is provided, it's used as the base; auto-appends a numeric suffix on collision.
func nextClipPath(src, customName string) (physical, virtual string) {
	rel, _ := filepath.Rel(*videoRoot, src)
	relDir := filepath.Dir(rel)
	ext := filepath.Ext(src)
	clipsDir := filepath.Join(*clipsRoot, relDir)
	os.MkdirAll(clipsDir, 0755)

	var fname string
	if customName != "" {
		// Use custom name; if it collides, append _2, _3, ...
		candidate := customName + ext
		n := 2
		for {
			if _, err := os.Stat(filepath.Join(clipsDir, candidate)); os.IsNotExist(err) {
				fname = candidate
				break
			}
			candidate = fmt.Sprintf("%s_%d%s", customName, n, ext)
			n++
		}
	} else {
		base := strings.TrimSuffix(filepath.Base(src), ext)
		re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `_clip(\d+)` + regexp.QuoteMeta(ext) + "$")
		entries, _ := os.ReadDir(clipsDir)
		max := 0
		for _, e := range entries {
			if m := re.FindStringSubmatch(e.Name()); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n > max {
					max = n
				}
			}
		}
		fname = fmt.Sprintf("%s_clip%02d%s", base, max+1, ext)
	}

	physical = filepath.Join(clipsDir, fname)
	virtual = filepath.ToSlash(filepath.Join("/", relDir, clipsSentinel, fname))
	return
}

// --- Clip editor handlers ---

func metaHandler(w http.ResponseWriter, r *http.Request) {
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	cacheDir := thumbCacheDir(src)
	metaPath := filepath.Join(cacheDir, "meta.json")

	// Regenerate if source is newer than cached meta
	srcInfo, err := os.Stat(src)
	if err != nil {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	metaInfo, err := os.Stat(metaPath)
	needsGen := os.IsNotExist(err) || (err == nil && srcInfo.ModTime().After(metaInfo.ModTime()))

	if needsGen {
		if err := generateSprites(src, cacheDir, defaultSpriteConfig()); err != nil {
			log.Printf("sprite generation failed for %s: %v", src, err)
			http.Error(w, "sprite generation failed: "+err.Error(), http.StatusInternalServerError)
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

func spriteHandler(w http.ResponseWriter, r *http.Request) {
	// /api/sprite?path=<video-rel-path>&sheet=sprite_001.jpg
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	sheet := filepath.Base(r.URL.Query().Get("sheet"))
	if !strings.HasPrefix(sheet, "sprite_") || !strings.HasSuffix(sheet, ".jpg") {
		http.Error(w, "bad sheet", http.StatusBadRequest)
		return
	}
	imgPath := filepath.Join(thumbCacheDir(src), sheet)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, imgPath)
}

func recoverHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	src, err := resolveInRoot(*videoRoot, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}

	ext := filepath.Ext(src)
	base := strings.TrimSuffix(src, ext)
	out := base + "_recovered" + ext

	// Try escalating recovery strategies.
	attempts := [][]string{
		{"-i", src, "-c", "copy", "-y", out},
		{"-fflags", "+genpts+discardcorrupt", "-i", src, "-c", "copy", "-y", out},
		{"-fflags", "+genpts+discardcorrupt+igndts+ignidx", "-err_detect", "ignore_err", "-i", src, "-c", "copy", "-y", out},
	}

	var lastErr string
	for _, args := range attempts {
		cmd := exec.Command(ffmpegBin, args...)
		if output, err := cmd.CombinedOutput(); err == nil {
			// Verify the recovered file has a valid duration
			if _, err := probeDuration(out); err == nil {
				clearBrokenMarker(src)
				rel, _ := filepath.Rel(*videoRoot, out)
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"recovered": "/" + filepath.ToSlash(rel)})
				return
			}
			os.Remove(out)
		} else {
			lastErr = strings.TrimSpace(string(output))
		}
	}

	http.Error(w, "recovery failed: "+lastErr, http.StatusInternalServerError)
}

// moveFile tries os.Rename first (fast, same-fs), falls back to copy+delete.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	os.Remove(src)
	return nil
}

type clipRequest struct {
	Path  string  `json:"path"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Name  string  `json:"name,omitempty"`
}

func clipHandler(w http.ResponseWriter, r *http.Request) {
	var req clipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	src, err := resolveInRoot(*videoRoot, req.Path)
	if err != nil || req.End <= req.Start {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	physOut, virtOut := nextClipPath(src, sanitizeClipName(req.Name))
	dur := req.End - req.Start

	// Write directly to the clips dir (under $HOME) — snap ffmpeg can access
	// home paths just fine, same as it does for sprite sheet generation.
	cmd := exec.Command(ffmpegBin,
		"-ss", fmt.Sprintf("%.3f", req.Start),
		"-i", src,
		"-t", fmt.Sprintf("%.3f", dur),
		"-c", "copy",
		"-y", physOut,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg clip failed: %v\n%s", err, output)
		http.Error(w, "ffmpeg failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	invalidateDirSizeCache(filepath.Dir(physOut))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"clip": virtOut})
}

func main() {
	flag.Parse()

	abs, err := filepath.Abs(*videoRoot)
	if err != nil {
		log.Fatal(err)
	}
	*videoRoot = abs

	if _, err := os.Stat(*videoRoot); os.IsNotExist(err) {
		log.Fatalf("Video directory does not exist: %s", *videoRoot)
	}

	cacheAbs, err := filepath.Abs(*cacheRoot)
	if err != nil {
		log.Fatal(err)
	}
	*cacheRoot = cacheAbs
	if err := os.MkdirAll(*cacheRoot, 0755); err != nil {
		log.Fatalf("Cannot create cache directory %s: %v", *cacheRoot, err)
	}
	log.Printf("Thumbnail cache: %s", *cacheRoot)

	clipsAbs, err := filepath.Abs(*clipsRoot)
	if err != nil {
		log.Fatal(err)
	}
	*clipsRoot = clipsAbs
	if err := os.MkdirAll(*clipsRoot, 0755); err != nil {
		log.Fatalf("Cannot create clips directory %s: %v", *clipsRoot, err)
	}
	log.Printf("Clips directory: %s", *clipsRoot)

	ffmpegBin = findBin("ffmpeg")
	ffprobeBin = findBin("ffprobe", "ffmpeg.ffprobe")
	if ffmpegBin == "" {
		log.Fatal("ffmpeg not found on PATH")
	}
	if ffprobeBin == "" {
		log.Fatal("ffprobe not found on PATH (tried ffprobe, ffmpeg.ffprobe)")
	}
	log.Printf("ffmpeg: %s", ffmpegBin)
	log.Printf("ffprobe: %s", ffprobeBin)

	loadSessions()

	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			loginPostHandler(w, r)
		} else {
			loginPageHandler(w, r)
		}
	})
	http.HandleFunc("/logout", logoutHandler)

	http.HandleFunc("/api/files", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteHandler(w, r)
		} else {
			listHandler(w, r)
		}
	}))

	http.HandleFunc("/api/video/meta", requireAuth(metaHandler))
	http.HandleFunc("/api/sprite/build-all", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			spriteBuildAllHandler(w, r)
		} else {
			spriteBuildAllProgressHandler(w, r)
		}
	}))
	http.HandleFunc("/api/sprite/build-folder", requireAuth(spriteBuildFolderHandler))
	http.HandleFunc("/api/recover", requireAuth(recoverHandler))
	http.HandleFunc("/api/queue", requireAuth(unifiedQueueHandler))
	http.HandleFunc("/api/search", requireAuth(searchHandler))
	http.HandleFunc("/api/info", requireAuth(infoHandler))
	http.HandleFunc("/api/sprite/queue", requireAuth(spriteQueueHandler))
	http.HandleFunc("/api/sprite/build", requireAuth(spriteBuildHandler))
	http.HandleFunc("/api/sprite/progress", requireAuth(spriteProgressHandler))
	http.HandleFunc("/api/sprite", requireAuth(spriteHandler))
	http.HandleFunc("/api/preview/build-all", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			previewBuildAllHandler(w, r)
		} else {
			previewBuildAllProgressHandler(w, r)
		}
	}))
	http.HandleFunc("/api/preview/build", requireAuth(previewBuildHandler))
	http.HandleFunc("/api/preview/progress", requireAuth(previewProgressHandler))
	http.HandleFunc("/api/preview/meta", requireAuth(previewMetaHandler))
	http.HandleFunc("/api/preview", requireAuth(previewImageHandler))
	http.HandleFunc("/api/clip", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clipHandler(w, r)
	}))

	http.HandleFunc("/editor", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "editor.html")
	}))

	http.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/files")
		if realDir, filename, isClips := splitAtClips(rel); isClips {
			absPath := filepath.Join(*clipsRoot, filepath.Clean("/"+realDir), filename)
			if !strings.HasPrefix(absPath, filepath.Clean(*clipsRoot)) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.ServeFile(w, r, absPath)
			return
		}
		rel = filepath.Clean("/" + rel)
		absPath := filepath.Join(*videoRoot, rel)
		if !strings.HasPrefix(absPath, filepath.Clean(*videoRoot)) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.ServeFile(w, r, absPath)
	})

	http.HandleFunc("/", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "index.html")
	}))

	// Kill all child ffmpeg processes on SIGTERM / SIGINT.
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("Shutting down — killing child processes…")
		killAllChildren()
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Video browser running at http://localhost%s", addr)
	log.Printf("Serving videos from: %s", *videoRoot)
	log.Printf("Password: %s", hardcodedPassword)
	log.Fatal(http.ListenAndServe(addr, nil))
}
