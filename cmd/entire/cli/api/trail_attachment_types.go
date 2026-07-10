package api

// TrailAttachment is a trail attachment (v1: images only). The server strips
// the internal r2_key; there is no URL field — a serve URL is built from the
// id and a variant. Width/Height/CreatedByUserID/DeletedAt are nullable.
type TrailAttachment struct {
	ID              string  `json:"id"`
	TrailID         string  `json:"trail_id"`
	RepoID          string  `json:"repo_id"`
	Kind            string  `json:"kind"`
	ContentType     string  `json:"content_type"`
	Filename        string  `json:"filename"`
	SizeBytes       int64   `json:"size_bytes"`
	Width           *int    `json:"width"`
	Height          *int    `json:"height"`
	CreatedByUserID *string `json:"created_by_user_id"`
	CreatedAt       string  `json:"created_at"`
	DeletedAt       *string `json:"deleted_at"`
}

// TrailAttachmentsResponse is the response from GET .../:number/attachments.
type TrailAttachmentsResponse struct {
	Attachments []TrailAttachment `json:"attachments"`
}

// TrailAttachmentUploadResponse is the response from POST .../:number/attachments.
type TrailAttachmentUploadResponse struct {
	Attachment TrailAttachment `json:"attachment"`
}
