package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestResumableUploadCompletesAcrossChunks(t *testing.T) {
	a := testApp(t)
	handler := a.routes()
	upload := createResumable(t, handler, "folder/large.bin", 11)

	appendChunk(t, handler, upload.ID, 0, "hello ", http.StatusOK)
	result := appendChunk(t, handler, upload.ID, 6, "world", http.StatusOK)
	if completed, _ := result["completed"].(bool); !completed {
		t.Fatalf("final chunk did not complete upload: %#v", result)
	}
	assertFileContents(t, filepath.Join(a.dataDir, "folder", "large.bin"), "hello world")
	if _, err := os.Stat(a.uploadMetaPath(upload.ID)); !os.IsNotExist(err) {
		t.Fatalf("upload metadata still exists: %v", err)
	}
}

func TestResumableUploadSurvivesNewAppInstance(t *testing.T) {
	a := testApp(t)
	handler := a.routes()
	upload := createResumable(t, handler, "archive.iso", 8)
	appendChunk(t, handler, upload.ID, 0, "1234", http.StatusOK)

	restarted := &app{
		dataDir:      a.dataDir,
		maxUpload:    a.maxUpload,
		authBypass:   true,
		allowedUsers: map[string]struct{}{},
		logger:       log.New(io.Discard, "", 0),
	}
	response := httptest.NewRecorder()
	restarted.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/uploads", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", response.Code, response.Body.String())
	}
	var listing struct {
		Uploads []resumableUpload `json:"uploads"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Uploads) != 1 || listing.Uploads[0].Offset != 4 {
		t.Fatalf("uploads after restart = %#v", listing.Uploads)
	}
	appendChunk(t, restarted.routes(), upload.ID, 4, "5678", http.StatusOK)
	assertFileContents(t, filepath.Join(a.dataDir, "archive.iso"), "12345678")
}

func TestCancelResumableUploadDeletesPartialData(t *testing.T) {
	a := testApp(t)
	handler := a.routes()
	upload := createResumable(t, handler, "cancel-me.bin", 10)
	appendChunk(t, handler, upload.ID, 0, "12345", http.StatusOK)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/uploads/"+upload.ID, nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d, body = %s", response.Code, response.Body.String())
	}
	for _, path := range []string{a.uploadMetaPath(upload.ID), a.uploadPartPath(upload.ID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", path, err)
		}
	}
}

func TestInterruptedChunkReportsStoredOffset(t *testing.T) {
	a := testApp(t)
	handler := a.routes()
	upload := createResumable(t, handler, "interrupted.bin", 10)

	request := httptest.NewRequest(http.MethodPatch, "/api/uploads/"+upload.ID, strings.NewReader("123"))
	request.ContentLength = 5
	request.Header.Set("Upload-Offset", "0")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		Offset int64 `json:"offset"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Offset != 3 {
		t.Fatalf("stored offset = %d, want 3", body.Offset)
	}
	appendChunk(t, handler, upload.ID, 3, "4567890", http.StatusOK)
	assertFileContents(t, filepath.Join(a.dataDir, "interrupted.bin"), "1234567890")
}

func TestPartialUploadsAreHiddenFromFileList(t *testing.T) {
	a := testApp(t)
	handler := a.routes()
	upload := createResumable(t, handler, "hidden.bin", 10)
	appendChunk(t, handler, upload.ID, 0, "123", http.StatusOK)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), upload.ID) || strings.Contains(response.Body.String(), "hidden.bin") {
		t.Fatalf("partial upload leaked into completed files: %s", response.Body.String())
	}
}

func createResumable(t *testing.T, handler http.Handler, path string, size int64) resumableUpload {
	t.Helper()
	payload, err := json.Marshal(createUploadRequest{Path: path, Size: size, LastModified: 1234})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/uploads", bytes.NewReader(payload)))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Upload resumableUpload `json:"upload"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Upload.ID == "" {
		t.Fatalf("create response has no upload: %s", response.Body.String())
	}
	return result.Upload
}

func appendChunk(t *testing.T, handler http.Handler, id string, offset int64, contents string, expectedStatus int) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPatch, "/api/uploads/"+id, strings.NewReader(contents))
	request.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != expectedStatus {
		t.Fatalf("append status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
