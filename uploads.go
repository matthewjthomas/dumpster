package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	resumableDirectory = ".dumpster-uploads"
	maxChunkSize       = int64(64 * 1024 * 1024)
)

type resumableUpload struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"`
	Size         int64     `json:"size"`
	Offset       int64     `json:"offset"`
	LastModified int64     `json:"lastModified"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type createUploadRequest struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	LastModified int64  `json:"lastModified"`
}

func (a *app) createUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var request createUploadRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid upload metadata."})
		return
	}
	request.Path = safeRelativePath(request.Path)
	if request.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The upload path is invalid."})
		return
	}
	if request.Size < 0 || request.Size > a.maxUpload {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The file exceeds the configured size limit."})
		return
	}
	if err := os.MkdirAll(a.resumableDir(), directoryMode); err != nil {
		a.serverError(w, "create resumable upload directory", err)
		return
	}

	id, err := uploadID()
	if err != nil {
		a.serverError(w, "create upload ID", err)
		return
	}
	now := time.Now().UTC()
	upload := resumableUpload{
		ID:           id,
		Path:         request.Path,
		Size:         request.Size,
		LastModified: request.LastModified,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	part, err := os.OpenFile(a.uploadPartPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		a.serverError(w, "create partial upload", err)
		return
	}
	if err := part.Chmod(fileMode); err != nil {
		part.Close()
		os.Remove(a.uploadPartPath(id))
		a.serverError(w, "set partial upload permissions", err)
		return
	}
	if err := part.Close(); err != nil {
		os.Remove(a.uploadPartPath(id))
		a.serverError(w, "close partial upload", err)
		return
	}
	if err := a.saveUpload(upload); err != nil {
		os.Remove(a.uploadPartPath(id))
		a.serverError(w, "save upload metadata", err)
		return
	}
	if upload.Size == 0 {
		result, err := a.completeUpload(upload)
		if err != nil {
			a.serverError(w, "complete empty upload", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"completed": true,
			"file":      result,
		})
		return
	}
	w.Header().Set("Location", "/api/uploads/"+id)
	writeJSON(w, http.StatusCreated, map[string]any{
		"completed": false,
		"upload":    upload,
	})
}

func (a *app) listUploads(w http.ResponseWriter, _ *http.Request) {
	uploads := make([]resumableUpload, 0)
	entries, err := os.ReadDir(a.resumableDir())
	if errors.Is(err, os.ErrNotExist) {
		writeJSON(w, http.StatusOK, map[string]any{"uploads": uploads})
		return
	}
	if err != nil {
		a.serverError(w, "list partial uploads", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		upload, err := a.loadUpload(id)
		if err != nil {
			a.logger.Printf("load partial upload %q: %v", id, err)
			continue
		}
		uploads = append(uploads, upload)
	}
	sortUploads(uploads)
	writeJSON(w, http.StatusOK, map[string]any{"uploads": uploads})
}

func (a *app) appendUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUploadID(id) {
		http.NotFound(w, r)
		return
	}
	lock := a.uploadLock(id)
	lock.Lock()
	defer lock.Unlock()

	upload, err := a.loadUpload(id)
	if errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		a.serverError(w, "load partial upload", err)
		return
	}
	expectedOffset, err := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || expectedOffset < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A valid Upload-Offset header is required."})
		return
	}
	if expectedOffset != upload.Offset {
		w.Header().Set("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "The upload offset has changed.",
			"offset": upload.Offset,
		})
		return
	}
	if r.ContentLength < 0 || r.ContentLength > maxChunkSize || upload.Offset+r.ContentLength > upload.Size {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The upload chunk has an invalid size."})
		return
	}

	part, err := os.OpenFile(a.uploadPartPath(id), os.O_WRONLY, fileMode)
	if err != nil {
		a.serverError(w, "open partial upload", err)
		return
	}
	actualOffset, err := part.Seek(0, io.SeekEnd)
	if err != nil {
		part.Close()
		a.serverError(w, "seek partial upload", err)
		return
	}
	if actualOffset != upload.Offset {
		upload.Offset = actualOffset
		part.Close()
		a.saveUpload(upload)
		w.Header().Set("Upload-Offset", strconv.FormatInt(actualOffset, 10))
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "The upload offset has changed.",
			"offset": actualOffset,
		})
		return
	}

	written, copyErr := io.Copy(part, io.LimitReader(r.Body, r.ContentLength))
	syncErr := part.Sync()
	closeErr := part.Close()
	upload.Offset += written
	upload.UpdatedAt = time.Now().UTC()
	saveErr := a.saveUpload(upload)
	if copyErr != nil || written != r.ContentLength || syncErr != nil || closeErr != nil || saveErr != nil {
		a.logger.Printf("write upload %q: copy=%v sync=%v close=%v save=%v", id, copyErr, syncErr, closeErr, saveErr)
		w.Header().Set("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  "The chunk was incomplete. Resume from the reported offset.",
			"offset": upload.Offset,
		})
		return
	}

	if upload.Offset == upload.Size {
		result, err := a.completeUpload(upload)
		if err != nil {
			a.serverError(w, "complete upload", err)
			return
		}
		a.uploadLocks.Delete(id)
		w.Header().Set("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
		writeJSON(w, http.StatusOK, map[string]any{
			"completed": true,
			"file":      result,
		})
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(upload.Offset, 10))
	writeJSON(w, http.StatusOK, map[string]any{
		"completed": false,
		"upload":    upload,
	})
}

func (a *app) cancelUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validUploadID(id) {
		http.NotFound(w, r)
		return
	}
	lock := a.uploadLock(id)
	lock.Lock()
	defer lock.Unlock()

	if _, err := a.loadUpload(id); errors.Is(err, os.ErrNotExist) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, "load partial upload", err)
		return
	}
	partErr := os.Remove(a.uploadPartPath(id))
	metaErr := os.Remove(a.uploadMetaPath(id))
	if partErr != nil && !errors.Is(partErr, os.ErrNotExist) {
		a.serverError(w, "remove partial upload", partErr)
		return
	}
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		a.serverError(w, "remove upload metadata", metaErr)
		return
	}
	a.uploadLocks.Delete(id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) completeUpload(upload resumableUpload) (uploadResult, error) {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()

	directory := filepath.Join(a.dataDir, filepath.Dir(filepath.FromSlash(upload.Path)))
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return uploadResult{}, fmt.Errorf("create destination directory: %w", err)
	}
	finalName, err := availableName(directory, path.Base(upload.Path))
	if err != nil {
		return uploadResult{}, err
	}
	finalPath := filepath.Join(directory, finalName)
	if err := os.Rename(a.uploadPartPath(upload.ID), finalPath); err != nil {
		return uploadResult{}, err
	}
	if err := os.Remove(a.uploadMetaPath(upload.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return uploadResult{}, err
	}
	storedPath, err := filepath.Rel(a.dataDir, finalPath)
	if err != nil {
		return uploadResult{}, err
	}
	return uploadResult{Name: filepath.ToSlash(storedPath), Size: upload.Size}, nil
}

func (a *app) loadUpload(id string) (resumableUpload, error) {
	var upload resumableUpload
	if !validUploadID(id) {
		return upload, os.ErrNotExist
	}
	data, err := os.ReadFile(a.uploadMetaPath(id))
	if err != nil {
		return upload, err
	}
	if err := json.Unmarshal(data, &upload); err != nil {
		return upload, err
	}
	info, err := os.Stat(a.uploadPartPath(id))
	if err != nil {
		return upload, err
	}
	upload.Offset = info.Size()
	return upload, nil
}

func (a *app) saveUpload(upload resumableUpload) error {
	data, err := json.Marshal(upload)
	if err != nil {
		return err
	}
	tmp, err := createSharedTemp(a.resumableDir(), ".metadata-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, a.uploadMetaPath(upload.ID))
}

func (a *app) uploadLock(id string) *sync.Mutex {
	value, _ := a.uploadLocks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func (a *app) resumableDir() string {
	return filepath.Join(a.dataDir, resumableDirectory)
}

func (a *app) uploadPartPath(id string) string {
	return filepath.Join(a.resumableDir(), id+".part")
}

func (a *app) uploadMetaPath(id string) string {
	return filepath.Join(a.resumableDir(), id+".json")
}

func uploadID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func validUploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func sortUploads(uploads []resumableUpload) {
	for i := 1; i < len(uploads); i++ {
		for j := i; j > 0 && uploads[j].UpdatedAt.After(uploads[j-1].UpdatedAt); j-- {
			uploads[j], uploads[j-1] = uploads[j-1], uploads[j]
		}
	}
}
