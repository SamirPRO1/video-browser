package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- Hardcoded password ---
const hardcodedPassword = "videos123"

var (
	port      = flag.Int("port", 2354, "Port to listen on")
	videoRoot = flag.String("dir", "./videos", "Root folder with video files")
)

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true,
	".mov": true, ".avi": true, ".m4v": true,
	".ts": true, ".ogv": true,
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
	// Already logged in — redirect to home
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
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size,omitempty"`
	ModTime string `json:"modTime,omitempty"`
}

func listHandler(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	rel = filepath.Clean("/" + rel)
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
		entry := Entry{Name: name, Path: entryPath, IsDir: e.IsDir()}
		if info != nil {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime().Format(time.RFC3339)
		}
		result = append(result, entry)
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
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

	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			loginPostHandler(w, r)
		} else {
			loginPageHandler(w, r)
		}
	})
	http.HandleFunc("/logout", logoutHandler)

	http.HandleFunc("/api/files", requireAuth(listHandler))

	http.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/files")
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
