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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

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

// --- Session store ---
var (
	sessions   = map[string]time.Time{}
	sessionsMu sync.Mutex
)

func newSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	token := hex.EncodeToString(b)
	sessionsMu.Lock()
	sessions[token] = time.Now().Add(24 * time.Hour)
	sessionsMu.Unlock()
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
			entry.Size = info.Size()
			entry.ModTime = info.ModTime().Format(time.RFC3339)
		}
		if !e.IsDir() {
			metaPath := filepath.Join(thumbCacheDir(absPath), "meta.json")
			_, err := os.Stat(metaPath)
			entry.HasSprites = err == nil
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
	rel := r.URL.Query().Get("path")
	rel = filepath.Clean("/" + rel)
	abs := filepath.Join(*videoRoot, rel)

	if !strings.HasPrefix(abs, filepath.Clean(*videoRoot)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
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

func generateSprites(src, cacheDir string) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}

	// Get duration via ffprobe
	out, err := exec.Command(ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		src,
	).Output()
	if err != nil {
		return fmt.Errorf("ffprobe: %w", err)
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 {
		return fmt.Errorf("bad duration: %q", strings.TrimSpace(string(out)))
	}

	// Generate sprite sheets
	spritePat := filepath.Join(cacheDir, "sprite_%03d.jpg")
	cmd := exec.Command(ffmpegBin,
		"-i", src,
		"-vf", "fps=1/2,scale=160:90:force_original_aspect_ratio=decrease,pad=160:90:(ow-iw)/2:(oh-ih)/2,tile=12x12",
		"-qscale:v", "5",
		"-y", spritePat,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg sprites: %w\n%s", err, out)
	}

	// Collect produced sheet filenames
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return err
	}
	var sheets []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sprite_") && strings.HasSuffix(e.Name(), ".jpg") {
			sheets = append(sheets, e.Name())
		}
	}
	sort.Strings(sheets)
	if len(sheets) == 0 {
		return fmt.Errorf("no sprite sheets produced")
	}

	meta := SpriteMeta{
		Duration:    duration,
		Interval:    2,
		Cols:        12,
		Rows:        12,
		ThumbWidth:  160,
		ThumbHeight: 90,
		Sheets:      sheets,
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(filepath.Join(cacheDir, "meta.json"), data, 0644)
}

// --- Sprite build job tracking ---

type spriteJob struct {
	Percent int    `json:"percent"`
	Done    bool   `json:"done"`
	Err     string `json:"err,omitempty"`
}

var (
	spriteJobs   = map[string]*spriteJob{}
	spriteJobsMu sync.Mutex
)

func getOrCreateJob(relPath string) (*spriteJob, bool) {
	spriteJobsMu.Lock()
	defer spriteJobsMu.Unlock()
	if j, ok := spriteJobs[relPath]; ok {
		return j, false // already exists
	}
	j := &spriteJob{}
	spriteJobs[relPath] = j
	return j, true // newly created
}

func generateSpritesAsync(relPath, src string, job *spriteJob) {
	cacheDir := thumbCacheDir(src)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, err.Error()
		spriteJobsMu.Unlock()
		return
	}

	// Get duration
	out, err := exec.Command(ffprobeBin,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		src,
	).Output()
	if err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, "ffprobe: "+err.Error()
		spriteJobsMu.Unlock()
		return
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || duration <= 0 {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, "bad duration"
		spriteJobsMu.Unlock()
		return
	}

	spritePat := filepath.Join(cacheDir, "sprite_%03d.jpg")
	cmd := exec.Command(ffmpegBin,
		"-progress", "pipe:1",
		"-nostats",
		"-i", src,
		"-vf", "fps=1/2,scale=160:90:force_original_aspect_ratio=decrease,pad=160:90:(ow-iw)/2:(oh-ih)/2,tile=12x12",
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

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "out_time_ms=") {
			ms, err := strconv.ParseInt(strings.TrimPrefix(line, "out_time_ms="), 10, 64)
			if err == nil && duration > 0 && ms > 0 {
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

	if err := cmd.Wait(); err != nil {
		spriteJobsMu.Lock()
		job.Done, job.Err = true, "ffmpeg: "+err.Error()
		spriteJobsMu.Unlock()
		return
	}

	// Collect sheets and write meta.json
	entries, _ := os.ReadDir(cacheDir)
	var sheets []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sprite_") && strings.HasSuffix(e.Name(), ".jpg") {
			sheets = append(sheets, e.Name())
		}
	}
	sort.Strings(sheets)

	meta := SpriteMeta{
		Duration:    duration,
		Interval:    2,
		Cols:        12,
		Rows:        12,
		ThumbWidth:  160,
		ThumbHeight: 90,
		Sheets:      sheets,
	}
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

	job, created := getOrCreateJob(relPath)
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

	spriteJobsMu.Lock()
	job, ok := spriteJobs[relPath]
	if ok {
		json.NewEncoder(w).Encode(job)
		spriteJobsMu.Unlock()
		return
	}
	spriteJobsMu.Unlock()

	// No active job — check if cache already exists
	src, err := resolveInRoot(*videoRoot, relPath)
	if err != nil {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	metaPath := filepath.Join(thumbCacheDir(src), "meta.json")
	w.Header().Set("Content-Type", "application/json")
	if _, err := os.Stat(metaPath); err == nil {
		json.NewEncoder(w).Encode(&spriteJob{Percent: 100, Done: true})
	} else {
		json.NewEncoder(w).Encode(&spriteJob{})
	}
}

// nextClipPath returns the physical save path and the virtual browser path for the next clip.
func nextClipPath(src string) (physical, virtual string) {
	rel, _ := filepath.Rel(*videoRoot, src)
	relDir := filepath.Dir(rel)
	ext := filepath.Ext(src)
	base := strings.TrimSuffix(filepath.Base(src), ext)
	re := regexp.MustCompile("^" + regexp.QuoteMeta(base) + `_clip(\d+)` + regexp.QuoteMeta(ext) + "$")

	clipsDir := filepath.Join(*clipsRoot, relDir)
	os.MkdirAll(clipsDir, 0755)

	entries, _ := os.ReadDir(clipsDir)
	max := 0
	for _, e := range entries {
		if m := re.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > max {
				max = n
			}
		}
	}
	fname := fmt.Sprintf("%s_clip%02d%s", base, max+1, ext)
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
		if err := generateSprites(src, cacheDir); err != nil {
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

	physOut, virtOut := nextClipPath(src)
	dur := req.End - req.Start

	tmpFile, err := os.CreateTemp("", "vbclip_*"+filepath.Ext(physOut))
	if err != nil {
		http.Error(w, "failed to create temp file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	cmd := exec.Command(ffmpegBin,
		"-ss", fmt.Sprintf("%.3f", req.Start),
		"-i", src,
		"-t", fmt.Sprintf("%.3f", dur),
		"-c", "copy",
		"-y", tmpPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("ffmpeg clip failed: %v\n%s", err, output)
		http.Error(w, "ffmpeg failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := moveFile(tmpPath, physOut); err != nil {
		log.Printf("move clip failed: %v", err)
		http.Error(w, "failed to save clip: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
	http.HandleFunc("/api/sprite/build", requireAuth(spriteBuildHandler))
	http.HandleFunc("/api/sprite/progress", requireAuth(spriteProgressHandler))
	http.HandleFunc("/api/sprite", requireAuth(spriteHandler))
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

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Video browser running at http://localhost%s", addr)
	log.Printf("Serving videos from: %s", *videoRoot)
	log.Printf("Password: %s", hardcodedPassword)
	log.Fatal(http.ListenAndServe(addr, nil))
}
