package service

import (
	"context"
	"encoding/json"
)

const (
	ImagePrimaryStatusQueued  = "queued"
	ImagePrimaryStatusRunning = "running"
	ImagePrimaryStatusSuccess = "success"
	ImagePrimaryStatusError   = "error"
)

type ImagePrimarySubmit struct {
	ClientTaskID string
	Payload      map[string]any
	Body         []byte
	ContentType  string
}

type ImagePrimarySnapshot struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Mode       string            `json:"mode"`
	Data       []json.RawMessage `json:"data,omitempty"`
	Response   json.RawMessage   `json:"response,omitempty"`
	Error      string            `json:"error,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
}

type ImagePrimaryClient interface {
	SubmitImages(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	SubmitEdits(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	SubmitResponses(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	GetTask(context.Context, string) (*ImagePrimarySnapshot, error)
}
