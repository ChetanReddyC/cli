package cli

import (
	"strings"
	"testing"
)

func TestTrailThreadPathBuilders(t *testing.T) {
	t.Parallel()
	if got := trailThreadsPath("gh", "acme", "widgets", 7); !strings.HasSuffix(got, "/7/threads") {
		t.Errorf("threads path = %q", got)
	}
	if got := trailThreadPath("gh", "acme", "widgets", 7, "th1"); !strings.HasSuffix(got, "/7/threads/th1") {
		t.Errorf("thread path = %q", got)
	}
	if got := trailThreadMessagesPath("gh", "acme", "widgets", 7, "th1"); !strings.HasSuffix(got, "/threads/th1/messages") {
		t.Errorf("messages path = %q", got)
	}
	if got := trailThreadMessagePath("gh", "acme", "widgets", 7, "th1", "m1"); !strings.HasSuffix(got, "/threads/th1/messages/m1") {
		t.Errorf("message path = %q", got)
	}
}

func TestTrailAttachmentPathBuilders(t *testing.T) {
	t.Parallel()
	if got := trailAttachmentsPath("gh", "acme", "widgets", 7); !strings.HasSuffix(got, "/7/attachments") {
		t.Errorf("attachments path = %q", got)
	}
	if got := trailAttachmentPath("gh", "acme", "widgets", 7, "a1"); !strings.HasSuffix(got, "/attachments/a1") {
		t.Errorf("attachment path = %q", got)
	}
}

func TestTrailWatchersPathUsesTrailID(t *testing.T) {
	t.Parallel()
	got := trailWatchersPath("uuid-123")
	if got != "/api/v1/trails/uuid-123/watchers" {
		t.Errorf("watchers path = %q", got)
	}
}

func TestDetectAttachmentContentType(t *testing.T) {
	t.Parallel()
	// PNG magic bytes.
	png := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}
	if ct, err := detectAttachmentContentType(png); err != nil || ct != "image/png" {
		t.Errorf("png: ct=%q err=%v", ct, err)
	}
	// JPEG magic bytes.
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}
	if ct, err := detectAttachmentContentType(jpeg); err != nil || ct != "image/jpeg" {
		t.Errorf("jpeg: ct=%q err=%v", ct, err)
	}
	// Plain text must be rejected.
	if _, err := detectAttachmentContentType([]byte("hello world, this is not an image")); err == nil {
		t.Error("expected non-image to be rejected")
	}
}

func TestTrailCommentSubtreeWiring(t *testing.T) {
	t.Parallel()
	cmd := newTrailCommentCmd()
	want := map[string]bool{"list": false, "show": false, "add": false, "reply": false, "edit": false, "delete": false, "resolve": false, "unresolve": false}
	for _, c := range cmd.Commands() {
		want[c.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("trail comment missing subcommand %q", name)
		}
	}
	if cmd.PersistentFlags().Lookup("trail") == nil || cmd.PersistentFlags().Lookup("branch") == nil {
		t.Error("trail comment missing --trail/--branch persistent flags")
	}
}

func TestTrailAttachmentSubtreeWiring(t *testing.T) {
	t.Parallel()
	cmd := newTrailAttachmentCmd()
	want := map[string]bool{"list": false, "add": false, "remove": false}
	for _, c := range cmd.Commands() {
		want[c.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("trail attachment missing subcommand %q", name)
		}
	}
}

func TestTrailWatchersCmdHasFlags(t *testing.T) {
	t.Parallel()
	cmd := newTrailWatchersCmd()
	if cmd.Flags().Lookup("json") == nil || cmd.Flags().Lookup("branch") == nil {
		t.Error("trail watchers missing --json/--branch flags")
	}
}
