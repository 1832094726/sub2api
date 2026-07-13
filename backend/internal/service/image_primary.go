package service

import (
	"context"
	"encoding/json"
	"time"
)

const (
	ImagePrimaryStatusQueued  = "queued"
	ImagePrimaryStatusRunning = "running"
	ImagePrimaryStatusSuccess = "success"
	ImagePrimaryStatusError   = "error"
)

const (
	ImagePrimarySettlementPending = "pending"
	ImagePrimarySettlementClaimed = "claimed"
	ImagePrimarySettlementSettled = "settled"
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
	Usage      json.RawMessage   `json:"usage,omitempty"`
	Error      string            `json:"error,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
}

type ImagePrimaryClient interface {
	SubmitImages(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	SubmitEdits(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	SubmitResponses(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error)
	GetTask(context.Context, string) (*ImagePrimarySnapshot, error)
}

type ImagePrimaryTask struct {
	ID                 int64
	PublicID           string
	UserID             int64
	APIKeyID           int64
	UsageLogID         *int64
	Protocol           string
	Model              string
	RequestHash        string
	UpstreamTaskID     *string
	Status             string
	FallbackReason     *string
	ResultLocator      *string
	ImageCount         int
	ImageSize          *string
	PrimaryDurationMS  int64
	FallbackDurationMS int64
	SettlementState    string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ExpiresAt          time.Time
}

type ImagePrimaryTaskCreate struct {
	PublicID    string
	UserID      int64
	APIKeyID    int64
	Protocol    string
	Model       string
	RequestHash string
	ExpiresAt   time.Time
}

type ImagePrimaryTaskTransition struct {
	UpstreamTaskID     *string
	FallbackReason     *string
	ResultLocator      *string
	ImageCount         int
	ImageSize          *string
	PrimaryDurationMS  int64
	FallbackDurationMS int64
}

type ImagePrimaryTaskRepository interface {
	CreateOrGet(context.Context, ImagePrimaryTaskCreate) (*ImagePrimaryTask, bool, error)
	GetByPublicID(context.Context, int64, int64, string) (*ImagePrimaryTask, error)
	BindUpstreamTask(context.Context, int64, string) (bool, error)
	Transition(context.Context, int64, string, string, ImagePrimaryTaskTransition) (bool, error)
	ClaimSettlement(context.Context, int64) (bool, error)
}
