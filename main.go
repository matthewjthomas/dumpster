package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxUploadMB int64 = 1024
	directoryMode            = 0o770
	fileMode                 = 0o660
)

//go:embed web/*
var webFiles embed.FS

type app struct {
	dataDir      string
	maxUpload    int64
	authBypass   bool
	allowedUsers map[string]struct{}
	logger       *log.Logger
	uploadMu     sync.Mutex
	uploadLocks  sync.Map
}

type fileInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type currentUser struct {
	Login       string `json:"login"`
	DisplayName string `json:"displayName"`
	AuthMethod  string `json:"authMethod"`
}

type uploadResult struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func main() {
	dataDir := envOr("DATA_DIR", "/data")
	maxUploadMB := envInt64("MAX_UPLOAD_MB", defaultMaxUploadMB)
	if maxUploadMB < 1 {
		log.Fatal("MAX_UPLOAD_MB must be greater than zero")
	}
	if err := os.MkdirAll(dataDir, directoryMode); err != nil {
		log.Fatalf("create data directory: %v", err)
	}

	a := &app{
		dataDir:      dataDir,
		maxUpload:    maxUploadMB * 1024 * 1024,
		authBypass:   envBool("AUTH_BYPASS"),
		allowedUsers: parseAllowedUsers(os.Getenv("ALLOWED_USERS")),
		logger:       log.New(os.Stdout, "", log.LstdFlags),
	}

	addr := envOr("LISTEN_ADDR", ":8080")
	server := &http.Server{
		Addr:              addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	a.logger.Printf("Dumpster listening on %s; storing files in %s", addr, dataDir)
	if a.authBypass {
		a.logger.Print("WARNING: AUTH_BYPASS is enabled; all clients can access Dumpster")
	}
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.Handle("GET /api/me", a.requireAuth(http.HandlerFunc(a.me)))
	mux.Handle("GET /api/tailscale/status", a.requireAuth(http.HandlerFunc(a.tailscaleStatus)))
	mux.Handle("GET /api/files", a.requireAuth(http.HandlerFunc(a.listFiles)))
	mux.Handle("POST /api/files", a.requireAuth(http.HandlerFunc(a.uploadFiles)))
	mux.Handle("GET /api/uploads", a.requireAuth(http.HandlerFunc(a.listUploads)))
	mux.Handle("POST /api/uploads", a.requireAuth(http.HandlerFunc(a.createUpload)))
	mux.Handle("PATCH /api/uploads/{id}", a.requireAuth(http.HandlerFunc(a.appendUpload)))
	mux.Handle("DELETE /api/uploads/{id}", a.requireAuth(http.HandlerFunc(a.cancelUpload)))
	mux.Handle("GET /files/{path...}", a.requireAuth(http.HandlerFunc(a.downloadFile)))

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", a.requireAuth(http.FileServer(http.FS(static))))
	return a.securityHeaders(mux)
}

func (a *app) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (a *app) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.authenticate(r)
		if !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "Access denied. Connect through this Dumpster's Tailscale Serve URL.",
			})
			return
		}
		r.Header.Set("Dumpster-User-Login", user.Login)
		r.Header.Set("Dumpster-User-Name", user.DisplayName)
		next.ServeHTTP(w, r)
	})
}

func (a *app) authenticate(r *http.Request) (currentUser, bool) {
	if a.authBypass {
		return currentUser{Login: "dev", DisplayName: "Development user", AuthMethod: "bypass"}, true
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return currentUser{}, false
	}
	login := strings.TrimSpace(r.Header.Get("Tailscale-User-Login"))
	if login == "" {
		return currentUser{}, false
	}
	if len(a.allowedUsers) > 0 {
		if _, ok := a.allowedUsers[strings.ToLower(login)]; !ok {
			return currentUser{}, false
		}
	}
	name := strings.TrimSpace(r.Header.Get("Tailscale-User-Name"))
	if name == "" {
		name = login
	}
	return currentUser{Login: login, DisplayName: name, AuthMethod: "tailscale"}, true
}

func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser{
		Login:       r.Header.Get("Dumpster-User-Login"),
		DisplayName: r.Header.Get("Dumpster-User-Name"),
		AuthMethod:  map[bool]string{true: "bypass", false: "tailscale"}[a.authBypass],
	})
}

func (a *app) tailscaleStatus(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	output, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false, "state": "Stopped"})
		return
	}
	var status struct {
		BackendState string `json:"BackendState"`
	}
	if err := json.Unmarshal(output, &status); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"connected": false, "state": "Unknown"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connected": status.BackendState == "Running",
		"state":     status.BackendState,
	})
}

func (a *app) listFiles(w http.ResponseWriter, _ *http.Request) {
	files := make([]fileInfo, 0)
	err := filepath.WalkDir(a.dataDir, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if filePath != a.dataDir && entry.Name() == resumableDirectory {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".dumpster-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(a.dataDir, filePath)
		if err != nil {
			return err
		}
		files = append(files, fileInfo{
			Name:       filepath.ToSlash(relative),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		a.serverError(w, "read data directory", err)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModifiedAt.After(files[j].ModifiedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (a *app) uploadFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, a.maxUpload+1024*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Expected multipart/form-data with one or more fields named files."})
		return
	}

	var uploaded []uploadResult
	var total int64
	var requestedPath string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			a.removeUploaded(uploaded)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The upload was incomplete or too large."})
			return
		}
		if part.FormName() == "path" && part.FileName() == "" {
			value, readErr := io.ReadAll(io.LimitReader(part, 4097))
			part.Close()
			if readErr != nil || len(value) > 4096 {
				a.removeUploaded(uploaded)
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The upload path is invalid."})
				return
			}
			requestedPath = string(value)
			continue
		}
		if part.FormName() != "files" || part.FileName() == "" {
			part.Close()
			continue
		}
		result, err := a.storePart(part, requestedPath, a.maxUpload-total)
		part.Close()
		if err != nil {
			a.removeUploaded(uploaded)
			status := http.StatusInternalServerError
			if errors.Is(err, errInvalidName) || errors.Is(err, errUploadTooLarge) {
				status = http.StatusBadRequest
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		total += result.Size
		uploaded = append(uploaded, result)
	}
	if len(uploaded) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No files were provided."})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"files": uploaded})
}

var (
	errInvalidName    = errors.New("a file has an invalid name")
	errUploadTooLarge = errors.New("upload exceeds the configured size limit")
)

func (a *app) storePart(part *multipart.Part, requestedPath string, remaining int64) (uploadResult, error) {
	if remaining <= 0 {
		return uploadResult{}, errUploadTooLarge
	}
	relativePath := ""
	if requestedPath != "" {
		relativePath = safeRelativePath(requestedPath)
		if relativePath == "" {
			return uploadResult{}, errInvalidName
		}
	} else {
		relativePath = safeRelativePath(part.FileName())
	}
	if relativePath == "" {
		return uploadResult{}, errInvalidName
	}
	tmp, err := createSharedTemp(a.dataDir, ".dumpster-upload-*")
	if err != nil {
		return uploadResult{}, fmt.Errorf("create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	written, copyErr := io.Copy(tmp, io.LimitReader(part, remaining+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return uploadResult{}, fmt.Errorf("write upload: %w", copyErr)
	}
	if closeErr != nil {
		return uploadResult{}, fmt.Errorf("close upload: %w", closeErr)
	}
	if written > remaining {
		return uploadResult{}, errUploadTooLarge
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	directory := filepath.Join(a.dataDir, filepath.Dir(filepath.FromSlash(relativePath)))
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return uploadResult{}, fmt.Errorf("create destination directory: %w", err)
	}
	finalName, err := availableName(directory, path.Base(relativePath))
	if err != nil {
		return uploadResult{}, fmt.Errorf("choose destination name: %w", err)
	}
	finalPath := filepath.Join(directory, finalName)
	if err := os.Rename(tmpName, finalPath); err != nil {
		return uploadResult{}, fmt.Errorf("save upload: %w", err)
	}
	storedPath, err := filepath.Rel(a.dataDir, finalPath)
	if err != nil {
		return uploadResult{}, fmt.Errorf("resolve saved upload: %w", err)
	}
	return uploadResult{Name: filepath.ToSlash(storedPath), Size: written}, nil
}

func (a *app) removeUploaded(files []uploadResult) {
	for _, file := range files {
		if err := os.Remove(filepath.Join(a.dataDir, file.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.logger.Printf("rollback upload %q: %v", file.Name, err)
		}
	}
}

func (a *app) downloadFile(w http.ResponseWriter, r *http.Request) {
	name := safeRelativePath(r.PathValue("path"))
	if name == "" {
		http.NotFound(w, r)
		return
	}
	filePath := filepath.Join(a.dataDir, filepath.FromSlash(name))
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
	http.ServeFile(w, r, filePath)
}

func (a *app) serverError(w http.ResponseWriter, operation string, err error) {
	a.logger.Printf("%s: %v", operation, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal server error."})
}

func safeRelativePath(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "/") {
		return ""
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return ""
	}
	for _, segment := range strings.Split(cleaned, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, ".dumpster-") {
			return ""
		}
	}
	return cleaned
}

func availableName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 0; i < 10_000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		_, err := os.Stat(filepath.Join(dir, candidate))
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("too many files with the same name")
}

func createSharedTemp(dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(fileMode); err != nil {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func parseAllowedUsers(value string) map[string]struct{} {
	users := make(map[string]struct{})
	for _, user := range strings.Split(value, ",") {
		if user = strings.ToLower(strings.TrimSpace(user)); user != "" {
			users[user] = struct{}{}
		}
	}
	return users
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return value == "1" || value == "true" || value == "yes"
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		log.Fatalf("%s must be an integer: %v", key, err)
	}
	return parsed
}
