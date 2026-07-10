package api

// TrailWatchersResponse is the response from GET /api/v1/trails/:trail_id/watchers.
// Watchers are the user IDs of clients currently connected to the trail's live
// event stream (presence-based, not a persisted subscription list).
type TrailWatchersResponse struct {
	Watchers []string `json:"watchers"`
}
