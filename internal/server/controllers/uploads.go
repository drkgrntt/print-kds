package controllers

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"print-kds/internal/board"

	"github.com/gofiber/fiber/v2"
)

// ErrBadUpload is returned (wrapped, with a user-facing message) when an
// uploaded file's type isn't allowed.
var ErrBadUpload = errors.New("unsupported upload")

// allowedUploadExts are the 3D-print file types a client can attach.
var allowedUploadExts = map[string]bool{
	".stl": true, ".3mf": true, ".obj": true, ".step": true, ".stp": true,
	".gcode": true, ".zip": true,
}

var unsafeFilenameChars = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

// sanitizeFilename keeps only a display-safe base name; the on-disk name
// is random (see saveUploads), so this only affects what's shown and sent
// back as the download name.
func sanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.TrimSpace(unsafeFilenameChars.ReplaceAllString(name, "_"))
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	return name
}

// saveUploads writes every file submitted under the multipart field
// "files" to <dir>/<tenantID>/<random><ext> - outside web/static, which is
// public, so downloads always go through an authenticated, tenant-checked
// route. The returned cleanup removes the written files again; call it if
// the order insert that was meant to record them fails.
func saveUploads(c *fiber.Ctx, dir string, tenantID int64) (files []board.NewOrderFile, cleanup func(), err error) {
	cleanup = func() {
		for _, f := range files {
			os.Remove(f.StoredPath)
		}
	}

	form, err := c.MultipartForm()
	if err != nil {
		return nil, cleanup, nil // not a multipart request: no files
	}

	tenantDir := filepath.Join(dir, fmt.Sprint(tenantID))
	if err := os.MkdirAll(tenantDir, 0o750); err != nil {
		return nil, cleanup, fmt.Errorf("create upload dir: %w", err)
	}

	for _, fh := range form.File["files"] {
		if fh.Filename == "" || fh.Size == 0 {
			continue // an empty file input submits a blank part
		}
		name := sanitizeFilename(fh.Filename)
		ext := strings.ToLower(filepath.Ext(name))
		if !allowedUploadExts[ext] {
			cleanup()
			return nil, func() {}, fmt.Errorf("%w: %q (allowed: .stl, .3mf, .obj, .step, .stp, .gcode, .zip)", ErrBadUpload, name)
		}

		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("generate file name: %w", err)
		}
		stored := filepath.Join(tenantDir, hex.EncodeToString(raw)+ext)
		if err := c.SaveFile(fh, stored); err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("save %s: %w", name, err)
		}
		files = append(files, board.NewOrderFile{
			Filename:    name,
			StoredPath:  stored,
			SizeBytes:   fh.Size,
			ContentType: fh.Header.Get(fiber.HeaderContentType),
		})
	}
	return files, cleanup, nil
}
