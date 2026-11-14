package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/sxi9/msp/internal/store"
)

// useFilesDir points the test server at a fresh temp MSP_FILES_DIR and returns
// it. The server reads s.cfg.FilesDir per request, so setting it after the
// handler was built still takes effect.
func (e *testEnv) useFilesDir() string {
	e.t.Helper()
	dir := e.t.TempDir()
	e.srv.cfg.FilesDir = dir
	return dir
}

// uploadFile posts a multipart/form-data upload. The name and org_id form fields
// are omitted when empty / nil (org_id nil = a global file). headers carries the
// cookie and CSRF header (build with withCookie).
func (e *testEnv) uploadFile(headers map[string]string, name string, orgID *int64, filename string, content []byte) (*http.Response, []byte) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if name != "" {
		_ = mw.WriteField("name", name)
	}
	if orgID != nil {
		_ = mw.WriteField("org_id", strconv.FormatInt(*orgID, 10))
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		e.t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(content); err != nil {
		e.t.Fatalf("write file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		e.t.Fatalf("close multipart: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+"/api/v1/files", &buf)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// fileFrom decodes the {"file":File} body of a 201 upload / 200 metadata.
func fileFrom(t *testing.T, body []byte) store.File {
	t.Helper()
	var out struct {
		File store.File `json:"file"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode file body %s: %v", body, err)
	}
	return out.File
}

func listFiles(t *testing.T, body []byte) []store.File {
	t.Helper()
	var out struct {
		Files []store.File `json:"files"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode files body %s: %v", body, err)
	}
	return out.Files
}

// TestFileLibrary covers the upload → list → metadata → delete flow, the stored
// bytes and hash, and the session/CSRF/role guards (SD1).
func TestFileLibrary(t *testing.T) {
	e := newTestEnv(t)
	dir := e.useFilesDir()
	admin := dashAdmin(e)
	viewer := dashUser(e, "viewer@example.com", RoleViewer)

	content := []byte("#!/bin/sh\necho marker > /tmp/deployed\n")
	wantSum := sha256.Sum256(content)
	wantHex := hex.EncodeToString(wantSum[:])

	// --- session / CSRF / role on the write -----------------------------------
	if resp, _ := e.uploadFile(nil, "x.sh", nil, "x.sh", content); resp.StatusCode != 401 {
		t.Fatalf("no session upload: %d", resp.StatusCode)
	}
	if resp, body := e.uploadFile(withCookie(admin, nil), "x.sh", nil, "x.sh", content); resp.StatusCode != 403 || errorCode(t, body) != "csrf" {
		t.Fatalf("no csrf upload: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.uploadFile(withCookie(viewer, csrfHeader), "x.sh", nil, "x.sh", content); resp.StatusCode != 403 || errorCode(t, body) != "forbidden" {
		t.Fatalf("viewer upload: %d %s", resp.StatusCode, body)
	}

	// --- a global upload (org_id omitted) by the admin -------------------------
	resp, body := e.uploadFile(withCookie(admin, csrfHeader), "installer.sh", nil, "artifact.sh", content)
	if resp.StatusCode != 201 {
		t.Fatalf("upload global: %d %s", resp.StatusCode, body)
	}
	gf := fileFrom(t, body)
	if gf.OrgID != nil {
		t.Fatalf("global file org_id = %v, want nil", gf.OrgID)
	}
	if gf.Name != "installer.sh" || gf.Size != int64(len(content)) || gf.SHA256 != wantHex {
		t.Fatalf("global file = %+v (want name installer.sh, size %d, sha %s)", gf, len(content), wantHex)
	}
	// Bytes landed at MSP_FILES_DIR/<id>.
	onDisk, err := os.ReadFile(e.srv.filePath(gf.ID))
	if err != nil || !bytes.Equal(onDisk, content) {
		t.Fatalf("bytes on disk at %s: %v (equal=%v)", e.srv.filePath(gf.ID), err, bytes.Equal(onDisk, content))
	}

	// --- a client-scoped upload to the env's own org --------------------------
	resp, body = e.uploadFile(withCookie(admin, csrfHeader), "patch.bin", &e.org.ID, "patch.bin", []byte("binary-blob"))
	if resp.StatusCode != 201 {
		t.Fatalf("upload scoped: %d %s", resp.StatusCode, body)
	}
	cf := fileFrom(t, body)
	if cf.OrgID == nil || *cf.OrgID != e.org.ID {
		t.Fatalf("scoped file org_id = %v, want %d", cf.OrgID, e.org.ID)
	}

	// --- list (admin sees both, newest first) ---------------------------------
	resp, body = e.do(http.MethodGet, "/api/v1/files", nil, withCookie(admin, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("list files: %d %s", resp.StatusCode, body)
	}
	files := listFiles(t, body)
	if len(files) != 2 || files[0].ID != cf.ID {
		t.Fatalf("list = %d files, newest %d (want 2, %d)", len(files), files[0].ID, cf.ID)
	}

	// --- metadata ------------------------------------------------------------
	resp, body = e.do(http.MethodGet, "/api/v1/files/"+itoa(gf.ID), nil, withCookie(viewer, nil))
	if resp.StatusCode != 200 {
		t.Fatalf("get file: %d %s", resp.StatusCode, body)
	}
	if got := fileFrom(t, body); got.ID != gf.ID || got.SHA256 != wantHex {
		t.Fatalf("metadata = %+v", got)
	}
	if resp, _ := e.do(http.MethodGet, "/api/v1/files/999999", nil, withCookie(admin, nil)); resp.StatusCode != 404 {
		t.Fatalf("unknown file metadata: %d", resp.StatusCode)
	}

	// --- delete removes the row and the disk file -----------------------------
	if resp, _ := e.do(http.MethodDelete, "/api/v1/files/"+itoa(gf.ID), nil, withCookie(viewer, csrfHeader)); resp.StatusCode != 403 {
		t.Fatalf("viewer delete: %d", resp.StatusCode)
	}
	if resp, body := e.do(http.MethodDelete, "/api/v1/files/"+itoa(gf.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 204 {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(e.srv.filePath(gf.ID)); !os.IsNotExist(err) {
		t.Fatalf("disk file still present after delete: %v", err)
	}
	resp, body = e.do(http.MethodGet, "/api/v1/files", nil, withCookie(admin, nil))
	if files := listFiles(t, body); len(files) != 1 || files[0].ID != cf.ID {
		t.Fatalf("after delete list = %+v", files)
	}
	if resp, _ := e.do(http.MethodDelete, "/api/v1/files/"+itoa(gf.ID), nil, withCookie(admin, csrfHeader)); resp.StatusCode != 404 {
		t.Fatalf("delete again: %d", resp.StatusCode)
	}

	// --- audit: two uploads and one delete ------------------------------------
	if n := len(e.auditActions("file.uploaded")); n != 2 {
		t.Fatalf("file.uploaded audit rows = %d, want 2", n)
	}
	if n := len(e.auditActions("file.deleted")); n != 1 {
		t.Fatalf("file.deleted audit rows = %d, want 1", n)
	}
	_ = dir
}

// TestFileUploadTooLarge: a file over MSP_MAX_FILE_MB is 413; one exactly at the
// cap is accepted (SD1).
func TestFileUploadTooLarge(t *testing.T) {
	e := newTestEnv(t)
	e.useFilesDir()
	e.srv.cfg.MaxFileMB = 1 // 1 MiB cap for the test
	admin := dashAdmin(e)

	atCap := bytes.Repeat([]byte("a"), 1<<20)
	if resp, body := e.uploadFile(withCookie(admin, csrfHeader), "ok.bin", nil, "ok.bin", atCap); resp.StatusCode != 201 {
		t.Fatalf("exactly-at-cap upload: %d %s", resp.StatusCode, body)
	}

	tooBig := bytes.Repeat([]byte("a"), 1<<20+1)
	resp, body := e.uploadFile(withCookie(admin, csrfHeader), "big.bin", nil, "big.bin", tooBig)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || errorCode(t, body) != "file_too_large" {
		t.Fatalf("over-cap upload: %d %s (want 413 file_too_large)", resp.StatusCode, body)
	}
}

// TestFileLibraryScoping is the RS2 proof for the staff file endpoints: a scoped
// operator cannot see, fetch metadata of, upload to or delete a file in another
// org, nor a global file, and cannot create a global file (403).
func TestFileLibraryScoping(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.useFilesDir()
	admin := dashAdmin(e)

	orgB, err := e.st.CreateOrg(ctx, "Beta Corp")
	if err != nil {
		t.Fatalf("CreateOrg: %v", err)
	}
	// A global file and a file in org B, both uploaded by the admin.
	_, body := e.uploadFile(withCookie(admin, csrfHeader), "global.sh", nil, "global.sh", []byte("g"))
	gf := fileFrom(t, body)
	_, body = e.uploadFile(withCookie(admin, csrfHeader), "beta.sh", &orgB.ID, "beta.sh", []byte("b"))
	bf := fileFrom(t, body)

	// A scoped operator who can see only the env's own org (A).
	op, _ := dashScopedUser(e, "scoped@example.com", RoleOperator, []int64{e.org.ID})

	// List: sees neither the global file nor org B's file (scoped excludes NULL).
	_, body = e.do(http.MethodGet, "/api/v1/files", nil, withCookie(op, nil))
	if files := listFiles(t, body); len(files) != 0 {
		t.Fatalf("scoped list = %+v, want empty", files)
	}
	// Metadata and delete of org B's file and the global file are 404.
	for _, id := range []int64{bf.ID, gf.ID} {
		if resp, b := e.do(http.MethodGet, "/api/v1/files/"+itoa(id), nil, withCookie(op, nil)); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
			t.Fatalf("scoped metadata file %d: %d %s", id, resp.StatusCode, b)
		}
		if resp, _ := e.do(http.MethodDelete, "/api/v1/files/"+itoa(id), nil, withCookie(op, csrfHeader)); resp.StatusCode != 404 {
			t.Fatalf("scoped delete file %d: %d", id, resp.StatusCode)
		}
	}
	// Upload to org B → 404 (hidden); a global upload → 403 (needs all-orgs);
	// upload to the visible org A → 201.
	if resp, b := e.uploadFile(withCookie(op, csrfHeader), "x.sh", &orgB.ID, "x.sh", []byte("x")); resp.StatusCode != 404 || errorCode(t, b) != "not_found" {
		t.Fatalf("scoped upload to org B: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.uploadFile(withCookie(op, csrfHeader), "x.sh", nil, "x.sh", []byte("x")); resp.StatusCode != 403 || errorCode(t, b) != "forbidden" {
		t.Fatalf("scoped global upload: %d %s", resp.StatusCode, b)
	}
	if resp, b := e.uploadFile(withCookie(op, csrfHeader), "ok.sh", &e.org.ID, "ok.sh", []byte("x")); resp.StatusCode != 201 {
		t.Fatalf("scoped upload to visible org: %d %s", resp.StatusCode, b)
	}
}
