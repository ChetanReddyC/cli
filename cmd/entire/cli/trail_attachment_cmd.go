package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
)

// trailAttachmentsPath builds the attachments collection path for a trail number.
func trailAttachmentsPath(forge, owner, repo string, number int) string {
	return trailNumberPath(forge, owner, repo, number) + "/attachments"
}

func trailAttachmentPath(forge, owner, repo string, number int, id string) string {
	return trailAttachmentsPath(forge, owner, repo, number) + "/" + id
}

// detectAttachmentContentType returns the image MIME type for a file's bytes,
// accepting only PNG and JPEG (the only kinds the server stores). It errors on
// anything else so the CLI fails fast with a clear message.
func detectAttachmentContentType(data []byte) (string, error) {
	ct := http.DetectContentType(data)
	switch ct {
	case "image/png", "image/jpeg":
		return ct, nil
	default:
		return "", fmt.Errorf("unsupported attachment type %q: only PNG and JPEG images are supported", ct)
	}
}

func newTrailAttachmentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachment",
		Short: "Manage image attachments on a trail",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.PersistentFlags().String("trail", "", "Trail number, id, or branch (defaults to the current branch)")
	cmd.PersistentFlags().String("branch", "", "Branch of the trail (defaults to current); cannot be combined with --trail")

	cmd.AddCommand(newTrailAttachmentListCmd())
	cmd.AddCommand(newTrailAttachmentAddCmd())
	cmd.AddCommand(newTrailAttachmentRemoveCmd())
	return cmd
}

func newTrailAttachmentListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List attachments on a trail",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withNumberedTrail(cmd, func(ctx context.Context, client *api.Client, found *api.TrailResource, forge, owner, repo string) error {
				resp, err := client.Get(ctx, trailAttachmentsPath(forge, owner, repo, found.Number))
				if err != nil {
					return fmt.Errorf("failed to list attachments: %w", err)
				}
				defer resp.Body.Close()
				if err := checkTrailResponse(resp); err != nil {
					return err
				}
				var out api.TrailAttachmentsResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode attachments response: %w", err)
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				if len(out.Attachments) == 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "No attachments on trail #%d\n", found.Number)
					return nil
				}
				for _, a := range out.Attachments {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %d bytes  %s\n", a.ID, a.Filename, a.ContentType, a.SizeBytes, a.CreatedAt)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

func newTrailAttachmentAddCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Upload an image attachment to a trail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]
			data, err := os.ReadFile(path) //nolint:gosec // user-supplied path is intentional
			if err != nil {
				return fmt.Errorf("failed to read %s: %w", path, err)
			}
			if len(data) == 0 {
				return fmt.Errorf("file %s is empty", path)
			}
			contentType, err := detectAttachmentContentType(data)
			if err != nil {
				return err
			}
			filename := filepath.Base(path)
			return withNumberedTrail(cmd, func(ctx context.Context, client *api.Client, found *api.TrailResource, forge, owner, repo string) error {
				uploadPath := trailAttachmentsPath(forge, owner, repo, found.Number) +
					"?filename=" + url.QueryEscape(filename) + "&kind=image"
				headers := http.Header{"Content-Type": []string{contentType}}
				resp, err := client.Request(ctx, http.MethodPost, uploadPath, headers, bytes.NewReader(data))
				if err != nil {
					return fmt.Errorf("failed to upload attachment: %w", err)
				}
				defer resp.Body.Close()
				if err := checkTrailResponse(resp); err != nil {
					return err
				}
				var out api.TrailAttachmentUploadResponse
				if err := api.DecodeJSON(resp, &out); err != nil {
					return fmt.Errorf("failed to decode upload response: %w", err)
				}
				if jsonOut {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Uploaded attachment %s (%s)\n", out.Attachment.ID, out.Attachment.Filename)
				fmt.Fprintf(cmd.OutOrStdout(), "  URL: %s\n", trailAttachmentURL(forge, owner, repo, out.Attachment.ID))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output as JSON")
	return cmd
}

// trailAttachmentURL builds the public inline-variant serve URL for an
// attachment. The host/owner/repo segments are decorative server-side; the id
// alone resolves the attachment.
func trailAttachmentURL(forge, owner, repo, id string) string {
	return strings.TrimRight(api.BaseURL(), "/") +
		"/api/v1/trails/" + forge + "/" + owner + "/" + repo + "/attachments/" + id + "/inline"
}

func newTrailAttachmentRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <attachment-id>",
		Short: "Delete an attachment from a trail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !force {
				return errors.New("refusing to delete attachment without confirmation; pass --force")
			}
			return withNumberedTrail(cmd, func(ctx context.Context, client *api.Client, found *api.TrailResource, forge, owner, repo string) error {
				resp, err := client.Delete(ctx, trailAttachmentPath(forge, owner, repo, found.Number, id))
				if err != nil {
					return fmt.Errorf("failed to delete attachment: %w", err)
				}
				defer resp.Body.Close()
				if err := checkTrailResponse(resp); err != nil {
					return err
				}
				// Success is 204 with an empty body — nothing to decode.
				fmt.Fprintf(cmd.OutOrStdout(), "Deleted attachment %s\n", id)
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Confirm deletion")
	return cmd
}
