package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type ImagePrimaryDecision string

const (
	ImagePrimarySuccess         ImagePrimaryDecision = "success"
	ImagePrimaryFallbackAllowed ImagePrimaryDecision = "fallback_allowed"
	ImagePrimaryPending         ImagePrimaryDecision = "pending"
	ImagePrimaryNotApplicable   ImagePrimaryDecision = "not_applicable"
)

type ImagePrimaryProtocol string

const (
	ImagePrimaryProtocolImages    ImagePrimaryProtocol = "images"
	ImagePrimaryProtocolEdits     ImagePrimaryProtocol = "edits"
	ImagePrimaryProtocolResponses ImagePrimaryProtocol = "responses"
)

var ErrImagePrimaryRequestConflict = errors.New("image primary task request hash mismatch")

type ImagePrimaryRouteRequest struct {
	PublicID    string
	UserID      int64
	APIKeyID    int64
	Protocol    ImagePrimaryProtocol
	Model       string
	RequestHash string
	ExpiresAt   time.Time
	Submit      *ImagePrimarySubmit
}

type ImagePrimaryRouteResult struct {
	Decision       ImagePrimaryDecision
	Task           *ImagePrimaryTask
	Snapshot       *ImagePrimarySnapshot
	FallbackReason string
	Err            error
}

type ImagePrimaryRouter struct {
	client       ImagePrimaryClient
	repository   ImagePrimaryTaskRepository
	enabled      bool
	timeout      time.Duration
	pollInterval time.Duration
}

func NewImagePrimaryRouter(
	client ImagePrimaryClient,
	repository ImagePrimaryTaskRepository,
	cfg *config.Config,
) *ImagePrimaryRouter {
	enabled := cfg != nil && cfg.ChatGPT2APIImage.PrimaryEnabled
	timeout := 300 * time.Second
	pollInterval := 5 * time.Second
	if cfg != nil {
		if cfg.ChatGPT2APIImage.TimeoutSeconds > 0 {
			timeout = time.Duration(cfg.ChatGPT2APIImage.TimeoutSeconds) * time.Second
		}
		if cfg.ChatGPT2APIImage.PollIntervalSeconds > 0 {
			pollInterval = time.Duration(cfg.ChatGPT2APIImage.PollIntervalSeconds) * time.Second
		}
	}
	return newImagePrimaryRouter(client, repository, enabled, timeout, pollInterval)
}

func newImagePrimaryRouter(
	client ImagePrimaryClient,
	repository ImagePrimaryTaskRepository,
	enabled bool,
	timeout, pollInterval time.Duration,
) *ImagePrimaryRouter {
	return &ImagePrimaryRouter{
		client: client, repository: repository, enabled: enabled,
		timeout: timeout, pollInterval: pollInterval,
	}
}

func (r *ImagePrimaryRouter) Route(ctx context.Context, request ImagePrimaryRouteRequest) ImagePrimaryRouteResult {
	if !r.enabled {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryNotApplicable}
	}
	if err := validateImagePrimaryRouteRequest(request); err != nil {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Err: err}
	}
	expiresAt := request.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(24 * time.Hour)
	}
	task, created, err := r.repository.CreateOrGet(ctx, ImagePrimaryTaskCreate{
		PublicID: request.PublicID, UserID: request.UserID, APIKeyID: request.APIKeyID,
		Protocol: string(request.Protocol), Model: request.Model,
		RequestHash: request.RequestHash, ExpiresAt: expiresAt,
	})
	if err != nil {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Err: err}
	}
	if task.RequestHash != "" && task.RequestHash != request.RequestHash {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Err: ErrImagePrimaryRequestConflict}
	}
	if !created {
		return r.resumeExisting(ctx, task)
	}

	transitioned, err := r.repository.Transition(
		ctx, task.ID, ImagePrimaryStatusQueued, ImagePrimaryStatusRunning, ImagePrimaryTaskTransition{},
	)
	if err != nil || !transitioned {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Err: err}
	}
	task.Status = ImagePrimaryStatusRunning
	snapshot, submitErr := r.submit(ctx, request.Protocol, request.Submit)
	if snapshot != nil && strings.TrimSpace(snapshot.ID) != "" {
		_, _ = r.repository.BindUpstreamTask(ctx, task.ID, snapshot.ID)
		task.UpstreamTaskID = &snapshot.ID
	}
	if submitErr != nil {
		lookup, lookupErr := r.client.GetTask(ctx, request.PublicID)
		if errors.Is(lookupErr, ErrImagePrimaryTaskNotFound) {
			return r.allowFallback(ctx, task, "primary_task_not_created", submitErr)
		}
		if lookupErr != nil {
			return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Err: submitErr}
		}
		snapshot = lookup
	}
	return r.resolveSnapshot(ctx, task, snapshot)
}

func (r *ImagePrimaryRouter) QueryOwnedTask(ctx context.Context, userID, apiKeyID int64, publicID string) (*ImagePrimaryTask, error) {
	return r.repository.GetByPublicID(ctx, userID, apiKeyID, publicID)
}

func (r *ImagePrimaryRouter) MarkSettled(ctx context.Context, taskID int64) error {
	_, err := r.repository.CompleteSettlement(ctx, taskID)
	return err
}

func (r *ImagePrimaryRouter) submit(ctx context.Context, protocol ImagePrimaryProtocol, submit *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	switch protocol {
	case ImagePrimaryProtocolImages:
		return r.client.SubmitImages(ctx, submit)
	case ImagePrimaryProtocolEdits:
		return r.client.SubmitEdits(ctx, submit)
	case ImagePrimaryProtocolResponses:
		return r.client.SubmitResponses(ctx, submit)
	default:
		return nil, fmt.Errorf("unsupported image primary protocol %q", protocol)
	}
}

func (r *ImagePrimaryRouter) resumeExisting(ctx context.Context, task *ImagePrimaryTask) ImagePrimaryRouteResult {
	switch task.Status {
	case ImagePrimaryStatusSuccess:
		return ImagePrimaryRouteResult{Decision: ImagePrimarySuccess, Task: task}
	case ImagePrimaryStatusError:
		return ImagePrimaryRouteResult{Decision: ImagePrimaryFallbackAllowed, Task: task, FallbackReason: valueOrEmpty(task.FallbackReason)}
	}
	snapshot, err := r.client.GetTask(ctx, task.PublicID)
	if err != nil {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Err: err}
	}
	return r.resolveSnapshot(ctx, task, snapshot)
}

func (r *ImagePrimaryRouter) resolveSnapshot(ctx context.Context, task *ImagePrimaryTask, snapshot *ImagePrimarySnapshot) ImagePrimaryRouteResult {
	for {
		if snapshot == nil {
			return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task}
		}
		switch snapshot.Status {
		case ImagePrimaryStatusSuccess:
			count := len(snapshot.Data)
			if snapshot.Mode == "response" {
				count = CountOpenAIResponseImageOutputs(snapshot.Response, snapshot.Events)
			}
			locator := snapshot.ID
			_, err := r.repository.Transition(ctx, task.ID, task.Status, ImagePrimaryStatusSuccess, ImagePrimaryTaskTransition{
				ResultLocator: &locator, ImageCount: count, ImageSize: optionalTrimmedStringPtr(snapshot.Size), PrimaryDurationMS: snapshot.DurationMS,
			})
			if err != nil {
				return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Snapshot: snapshot, Err: err}
			}
			task.Status = ImagePrimaryStatusSuccess
			return ImagePrimaryRouteResult{Decision: ImagePrimarySuccess, Task: task, Snapshot: snapshot}
		case ImagePrimaryStatusError:
			return r.allowFallback(ctx, task, "primary_error", errors.New(snapshot.Error))
		case ImagePrimaryStatusQueued, ImagePrimaryStatusRunning:
		default:
			return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Snapshot: snapshot, Err: fmt.Errorf("unknown primary task status %q", snapshot.Status)}
		}
		break
	}

	pollCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-pollCtx.Done():
			return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Snapshot: snapshot, Err: pollCtx.Err()}
		case <-ticker.C:
			latest, err := r.client.GetTask(pollCtx, task.PublicID)
			if err != nil {
				if errors.Is(err, ErrImagePrimaryTaskNotFound) {
					continue
				}
				return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Snapshot: snapshot, Err: err}
			}
			snapshot = latest
			if snapshot.Status == ImagePrimaryStatusQueued || snapshot.Status == ImagePrimaryStatusRunning {
				continue
			}
			return r.resolveSnapshot(pollCtx, task, snapshot)
		}
	}
}

func (r *ImagePrimaryRouter) allowFallback(ctx context.Context, task *ImagePrimaryTask, reason string, cause error) ImagePrimaryRouteResult {
	_, err := r.repository.Transition(ctx, task.ID, task.Status, ImagePrimaryStatusError, ImagePrimaryTaskTransition{
		FallbackReason: &reason,
	})
	if err != nil {
		return ImagePrimaryRouteResult{Decision: ImagePrimaryPending, Task: task, Err: err}
	}
	task.Status = ImagePrimaryStatusError
	task.FallbackReason = &reason
	return ImagePrimaryRouteResult{
		Decision: ImagePrimaryFallbackAllowed, Task: task,
		FallbackReason: reason, Err: cause,
	}
}

func validateImagePrimaryRouteRequest(request ImagePrimaryRouteRequest) error {
	if strings.TrimSpace(request.PublicID) == "" || strings.TrimSpace(request.RequestHash) == "" {
		return errors.New("image primary public ID and request hash are required")
	}
	if request.UserID <= 0 || request.APIKeyID <= 0 {
		return errors.New("image primary owner is required")
	}
	if request.Submit == nil {
		return errors.New("image primary submit payload is required")
	}
	return nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func ProvideChatGPT2APIImageClient(cfg *config.Config) (ImagePrimaryClient, error) {
	if cfg == nil || !cfg.ChatGPT2APIImage.PrimaryEnabled {
		return disabledImagePrimaryClient{}, nil
	}
	timeout := 310 * time.Second
	if cfg.ChatGPT2APIImage.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.ChatGPT2APIImage.TimeoutSeconds+10) * time.Second
	}
	return NewChatGPT2APIImageClient(ChatGPT2APIImageClientConfig{
		BaseURL:    cfg.ChatGPT2APIImage.BaseURL,
		APIKey:     cfg.ChatGPT2APIImage.APIKey,
		Model:      cfg.ChatGPT2APIImage.Model,
		HTTPClient: &http.Client{Timeout: timeout},
	})
}

type disabledImagePrimaryClient struct{}

func (disabledImagePrimaryClient) SubmitImages(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	return nil, errors.New("image primary routing is disabled")
}
func (disabledImagePrimaryClient) SubmitEdits(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	return nil, errors.New("image primary routing is disabled")
}
func (disabledImagePrimaryClient) SubmitResponses(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	return nil, errors.New("image primary routing is disabled")
}
func (disabledImagePrimaryClient) GetTask(context.Context, string) (*ImagePrimarySnapshot, error) {
	return nil, errors.New("image primary routing is disabled")
}
