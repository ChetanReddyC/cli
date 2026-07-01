package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Compile-time interface assertion.
var _ agent.SidecarImageProvider = (*CursorAgent)(nil)

// cursorChatsDirEnv overrides the base directory that holds Cursor's per-session
// SQLite blob stores. Used by tests and mock environments.
const cursorChatsDirEnv = "ENTIRE_TEST_CURSOR_CHATS_DIR"

// storeDBBlobQuery selects the hex encoding of every blob whose leading bytes
// match a known image magic number (JPEG, PNG, GIF, or RIFF/WEBP). sqlite3's
// hex() returns uppercase, so the literals are uppercase.
const storeDBBlobQuery = "SELECT hex(data) FROM blobs WHERE " +
	"substr(hex(data),1,6)='FFD8FF' OR " + // JPEG
	"substr(hex(data),1,8)='89504E47' OR " + // PNG
	"substr(hex(data),1,8)='47494638' OR " + // GIF
	"(substr(hex(data),1,8)='52494646' AND substr(hex(data),17,8)='57454250');" // RIFF....WEBP

// SidecarImages captures images that Cursor stores outside the JSONL transcript.
// Cursor keeps pasted/generated images in a per-session SQLite blob store
// (~/.cursor/chats/<workspace>/<session>/store.db), not the transcript Entire
// condenses, so they would otherwise be lost from the checkpoint. This locates
// that store for the session, shells out to the sqlite3 binary to read the image
// blobs, and returns them as checkpoint assets.
//
// It is best-effort: when the store, the sqlite3 binary, or the expected schema
// is absent it returns no images and no error. sessionRef is the transcript path.
func (c *CursorAgent) SidecarImages(ctx context.Context, sessionRef string) ([]agent.CompactedTranscriptAsset, error) {
	logCtx := logging.WithComponent(ctx, "agent.cursor")

	sessionID := sessionIDFromTranscriptPath(sessionRef)
	if sessionID == "" {
		return nil, nil
	}

	dbPath, err := findStoreDB(sessionID)
	if err != nil {
		return nil, fmt.Errorf("locate cursor store.db: %w", err)
	}
	if dbPath == "" {
		return nil, nil // no sidecar store for this session
	}

	if !sqlite3Available() {
		logging.Debug(logCtx, "sqlite3 not found; skipping cursor sidecar image capture")
		return nil, nil
	}

	hexBlobs, err := readImageBlobs(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("read cursor store.db blobs: %w", err)
	}

	assets := make([]agent.CompactedTranscriptAsset, 0, len(hexBlobs))
	seen := make(map[string]struct{}, len(hexBlobs))
	for _, h := range hexBlobs {
		data, err := hex.DecodeString(h)
		if err != nil {
			logging.Debug(logCtx, "skipping undecodable cursor blob", slog.String("error", err.Error()))
			continue
		}
		mediaType, ext := detectImageType(data)
		if mediaType == "" {
			continue // not an image after all
		}
		sum := sha256.Sum256(data)
		name := fmt.Sprintf("img-%s.%s", hex.EncodeToString(sum[:16]), ext)
		if _, dup := seen[name]; dup {
			continue // identical image already captured
		}
		seen[name] = struct{}{}
		assets = append(assets, agent.CompactedTranscriptAsset{
			Name:      name,
			MediaType: mediaType,
			Data:      data,
		})
	}

	if len(assets) > 0 {
		logging.Debug(logCtx, "captured cursor sidecar images",
			slog.Int("count", len(assets)), slog.String("session", sessionID))
	}
	return assets, nil
}

// sessionIDFromTranscriptPath extracts the Cursor session id from a transcript
// path. Both the nested (<id>/<id>.jsonl) and flat (<id>.jsonl) layouts name the
// file after the session id, so the base name without extension is the id.
func sessionIDFromTranscriptPath(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	base := filepath.Base(transcriptPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// findStoreDB locates the SQLite blob store for a session. Cursor lays these out
// as <chats>/<workspace-hash>/<session-id>/store.db; the workspace hash is not
// derivable from the session id, so we glob across workspaces. Returns "" when
// no store exists (a session with no sidecar images).
func findStoreDB(sessionID string) (string, error) {
	base := os.Getenv(cursorChatsDirEnv)
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("get home directory: %w", err)
		}
		base = filepath.Join(home, ".cursor", "chats")
	}

	matches, err := filepath.Glob(filepath.Join(base, "*", sessionID, "store.db"))
	if err != nil {
		return "", fmt.Errorf("glob store.db: %w", err)
	}
	if len(matches) == 0 {
		return "", nil
	}
	return matches[0], nil
}

// readImageBlobs copies the store to a temp location (so a live Cursor session
// cannot lock or mutate it mid-read, and any WAL is applied) and shells out to
// sqlite3 to select image blobs as hex. Returns one hex string per image blob.
func readImageBlobs(ctx context.Context, dbPath string) ([]string, error) {
	tmpDir, err := os.MkdirTemp("", "entire-cursor-store-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	tmpDB := filepath.Join(tmpDir, "store.db")
	if err := copyFile(dbPath, tmpDB); err != nil {
		return nil, fmt.Errorf("copy store.db: %w", err)
	}
	// Copy the WAL/SHM sidecars if present so committed-but-not-checkpointed
	// pages are applied when sqlite3 opens the copy. Best-effort: a missing or
	// uncopyable sidecar just means we read the main db as-is.
	for _, suffix := range []string{"-wal", "-shm"} {
		src := dbPath + suffix
		if !fileExists(src) {
			continue
		}
		if err := copyFile(src, tmpDB+suffix); err != nil {
			logging.Debug(ctx, "skipping cursor store.db sidecar copy",
				slog.String("file", src), slog.String("error", err.Error()))
		}
	}

	cmd := exec.CommandContext(ctx, "sqlite3", tmpDB, storeDBBlobQuery)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("sqlite3 query failed: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("sqlite3 query failed: %w", err)
	}

	var blobs []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			blobs = append(blobs, line)
		}
	}
	return blobs, nil
}

// detectImageType returns the media type and file extension for known image
// magic bytes, or ("", "") when the bytes are not a recognized image.
func detectImageType(data []byte) (mediaType, ext string) {
	switch {
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", "png"
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg", "jpg"
	case len(data) >= 6 && string(data[:6]) == "GIF89a", len(data) >= 6 && string(data[:6]) == "GIF87a":
		return "image/gif", "gif"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp", "webp"
	default:
		return "", ""
	}
}

func sqlite3Available() bool {
	_, err := exec.LookPath("sqlite3")
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // path is an internal, non-user-controlled store location
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // dst is a temp file we created
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy contents: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	return nil
}
