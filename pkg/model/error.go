package model

type ErrorResponse struct {
	Error  string `json:"error"`
	RoomID string `json:"roomId,omitempty"`
}
