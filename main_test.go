package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testApp(t *testing.T) *app {
	t.Helper()
	return &app{
		dataDir:      t.TempDir(),
		maxUpload:    1024 * 1024,
		authBypass:   true,
		allowedUsers: map[string]struct{}{},
		logger:       log.New(io.Discard, "", 0),
	}
}

func multipartRequest(t *testing.T, files map[string]string) *http.Request {
	return multipartPathRequest(t, "", files)
}

func multipartPathRequest(t *testing.T, uploadPath string, files map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if uploadPath != "" {
		if err := writer.WriteField("path", uploadPath); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range files {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func TestUploadListAndDownload(t *testing.T) {
	a := testApp(t)
	handler := a.routes()

	request := multipartRequest(t, map[string]string{
		"notes.txt": "one",
		"photo.jpg": "two",
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}

	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status = %d", listResponse.Code)
	}
	var listing struct {
		Files []fileInfo `json:"files"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(listing.Files))
	}

	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, httptest.NewRequest(http.MethodGet, "/files/notes.txt", nil))
	if downloadResponse.Code != http.StatusOK || downloadResponse.Body.String() != "one" {
		t.Fatalf("download = (%d, %q)", downloadResponse.Code, downloadResponse.Body.String())
	}
}

func TestFolderUploadPreservesRelativePath(t *testing.T) {
	a := testApp(t)
	handler := a.routes()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, multipartPathRequest(t, "project/assets/logo.txt", map[string]string{
		"logo.txt": "folder contents",
	}))
	if response.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	assertFileContents(t, filepath.Join(a.dataDir, "project", "assets", "logo.txt"), "folder contents")

	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, httptest.NewRequest(http.MethodGet, "/api/files", nil))
	if !bytes.Contains(listResponse.Body.Bytes(), []byte(`"project/assets/logo.txt"`)) {
		t.Fatalf("folder path missing from listing: %s", listResponse.Body.String())
	}

	downloadResponse := httptest.NewRecorder()
	handler.ServeHTTP(downloadResponse, httptest.NewRequest(http.MethodGet, "/files/project/assets/logo.txt", nil))
	if downloadResponse.Code != http.StatusOK || downloadResponse.Body.String() != "folder contents" {
		t.Fatalf("download = (%d, %q)", downloadResponse.Code, downloadResponse.Body.String())
	}
}

func TestFolderUploadRejectsTraversal(t *testing.T) {
	a := testApp(t)
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, multipartPathRequest(t, "../outside.txt", map[string]string{
		"inside.txt": "contents",
	}))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid requested path status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(a.dataDir), "outside.txt")); !os.IsNotExist(err) {
		t.Fatal("upload escaped the data directory")
	}
}

func TestDuplicateNamesArePreserved(t *testing.T) {
	a := testApp(t)
	handler := a.routes()

	for _, contents := range []string{"first", "second"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, multipartRequest(t, map[string]string{"report.pdf": contents}))
		if response.Code != http.StatusCreated {
			t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
		}
	}

	assertFileContents(t, filepath.Join(a.dataDir, "report.pdf"), "first")
	assertFileContents(t, filepath.Join(a.dataDir, "report (1).pdf"), "second")
}

func TestUploadLimitRollsBackRequest(t *testing.T) {
	a := testApp(t)
	a.maxUpload = 5
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, multipartRequest(t, map[string]string{
		"small.txt": "123",
		"large.txt": "123456",
	}))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name()[0] != '.' {
			t.Fatalf("upload was not rolled back: %s exists", entry.Name())
		}
	}
}

func TestTailscaleHeadersOnlyTrustedFromLoopback(t *testing.T) {
	a := testApp(t)
	a.authBypass = false
	handler := a.routes()

	spoofed := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	spoofed.Header.Set("Tailscale-User-Login", "user@example.com")
	spoofedResponse := httptest.NewRecorder()
	handler.ServeHTTP(spoofedResponse, spoofed)
	if spoofedResponse.Code != http.StatusForbidden {
		t.Fatalf("spoofed request status = %d", spoofedResponse.Code)
	}

	proxied := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	proxied.RemoteAddr = "127.0.0.1:12345"
	proxied.Header.Set("Tailscale-User-Login", "user@example.com")
	proxied.Header.Set("Tailscale-User-Name", "Example User")
	proxiedResponse := httptest.NewRecorder()
	handler.ServeHTTP(proxiedResponse, proxied)
	if proxiedResponse.Code != http.StatusOK {
		t.Fatalf("proxied request status = %d, body = %s", proxiedResponse.Code, proxiedResponse.Body.String())
	}
}

func TestAllowedUsers(t *testing.T) {
	a := testApp(t)
	a.authBypass = false
	a.allowedUsers = parseAllowedUsers("allowed@example.com")
	handler := a.routes()

	for login, expected := range map[string]int{
		"allowed@example.com": http.StatusOK,
		"other@example.com":   http.StatusForbidden,
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("Tailscale-User-Login", login)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Errorf("%s: status = %d, want %d", login, response.Code, expected)
		}
	}
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != expected {
		t.Fatalf("%s = %q, want %q", path, contents, expected)
	}
}
