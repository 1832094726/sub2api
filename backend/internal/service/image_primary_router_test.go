package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeImagePrimaryClient struct {
	snapshot    *ImagePrimarySnapshot
	submitErr   error
	lookup      *ImagePrimarySnapshot
	lookupErr   error
	submitCalls int
}

func (f *fakeImagePrimaryClient) SubmitImages(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	f.submitCalls++
	return f.snapshot, f.submitErr
}
func (f *fakeImagePrimaryClient) SubmitEdits(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	f.submitCalls++
	return f.snapshot, f.submitErr
}
func (f *fakeImagePrimaryClient) SubmitResponses(context.Context, *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	f.submitCalls++
	return f.snapshot, f.submitErr
}
func (f *fakeImagePrimaryClient) GetTask(context.Context, string) (*ImagePrimarySnapshot, error) {
	return f.lookup, f.lookupErr
}

type fakeImagePrimaryTaskRepository struct {
	task           *ImagePrimaryTask
	created        bool
	lastTransition ImagePrimaryTaskTransition
}

func (f *fakeImagePrimaryTaskRepository) CreateOrGet(context.Context, ImagePrimaryTaskCreate) (*ImagePrimaryTask, bool, error) {
	return f.task, f.created, nil
}
func (f *fakeImagePrimaryTaskRepository) GetByPublicID(context.Context, int64, int64, string) (*ImagePrimaryTask, error) {
	return f.task, nil
}
func (f *fakeImagePrimaryTaskRepository) BindUpstreamTask(_ context.Context, _ int64, upstreamTaskID string) (bool, error) {
	f.task.UpstreamTaskID = &upstreamTaskID
	return true, nil
}
func (f *fakeImagePrimaryTaskRepository) Transition(_ context.Context, _ int64, from, to string, transition ImagePrimaryTaskTransition) (bool, error) {
	if f.task.Status != from {
		return false, nil
	}
	f.lastTransition = transition
	f.task.Status = to
	return true, nil
}
func (f *fakeImagePrimaryTaskRepository) ClaimSettlement(context.Context, int64) (bool, error) {
	return true, nil
}
func (f *fakeImagePrimaryTaskRepository) CompleteSettlement(context.Context, int64) (bool, error) {
	return true, nil
}

func TestImagePrimaryRouterFallbackPolicy(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   *ImagePrimarySnapshot
		submitErr  error
		lookup     *ImagePrimarySnapshot
		lookupErr  error
		want       ImagePrimaryDecision
		wantReason string
	}{
		{name: "success", snapshot: &ImagePrimarySnapshot{ID: "imgp_1", Status: "success"}, want: ImagePrimarySuccess},
		{name: "explicit error", snapshot: &ImagePrimarySnapshot{ID: "imgp_1", Status: "error", Error: "rejected"}, want: ImagePrimaryFallbackAllowed, wantReason: "primary_error"},
		{name: "running timeout", snapshot: &ImagePrimarySnapshot{ID: "imgp_1", Status: "running"}, lookup: &ImagePrimarySnapshot{ID: "imgp_1", Status: "running"}, want: ImagePrimaryPending},
		{name: "connection uncertain", submitErr: io.ErrUnexpectedEOF, lookupErr: errors.New("lookup unavailable"), want: ImagePrimaryPending},
		{name: "confirmed missing", submitErr: io.ErrUnexpectedEOF, lookupErr: ErrImagePrimaryTaskNotFound, want: ImagePrimaryFallbackAllowed, wantReason: "primary_task_not_created"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeImagePrimaryClient{
				snapshot: tt.snapshot, submitErr: tt.submitErr,
				lookup: tt.lookup, lookupErr: tt.lookupErr,
			}
			repo := &fakeImagePrimaryTaskRepository{
				task:    &ImagePrimaryTask{ID: 1, PublicID: "imgp_1", Status: ImagePrimaryStatusQueued},
				created: true,
			}
			router := newImagePrimaryRouter(client, repo, true, 10*time.Millisecond, time.Millisecond)
			got := router.Route(context.Background(), ImagePrimaryRouteRequest{
				PublicID: "imgp_1", UserID: 7, APIKeyID: 9, Protocol: ImagePrimaryProtocolImages,
				Model: "gpt-image-2", RequestHash: "hash-1",
				Submit: &ImagePrimarySubmit{ClientTaskID: "imgp_1", Payload: map[string]any{"prompt": "redacted"}},
			})
			require.Equal(t, tt.want, got.Decision)
			require.Equal(t, tt.wantReason, got.FallbackReason)
			require.LessOrEqual(t, client.submitCalls, 1)
		})
	}
}

func TestImagePrimaryRouterExistingTaskDoesNotSubmitAgain(t *testing.T) {
	client := &fakeImagePrimaryClient{
		lookup: &ImagePrimarySnapshot{ID: "imgp_existing", Status: ImagePrimaryStatusSuccess},
	}
	repo := &fakeImagePrimaryTaskRepository{
		task: &ImagePrimaryTask{
			ID: 1, PublicID: "imgp_existing", Status: ImagePrimaryStatusRunning,
			RequestHash: "hash-existing",
		},
		created: false,
	}
	router := newImagePrimaryRouter(client, repo, true, 10*time.Millisecond, time.Millisecond)

	result := router.Route(context.Background(), ImagePrimaryRouteRequest{
		PublicID: "imgp_existing", UserID: 7, APIKeyID: 9,
		Protocol: ImagePrimaryProtocolImages, Model: "gpt-image-2", RequestHash: "hash-existing",
		Submit: &ImagePrimarySubmit{ClientTaskID: "imgp_existing", Payload: map[string]any{}},
	})

	require.Equal(t, ImagePrimarySuccess, result.Decision)
	require.Zero(t, client.submitCalls)
}

func TestImagePrimaryRouterCountsResponsesFinalImages(t *testing.T) {
	client := &fakeImagePrimaryClient{snapshot: &ImagePrimarySnapshot{
		ID: "imgp_response", Status: ImagePrimaryStatusSuccess, Mode: "response",
		Events: []json.RawMessage{json.RawMessage(`{"type":"response.output_item.done","item":{"type":"image_generation_call","id":"call_1","result":"final-image"}}`)},
	}}
	repo := &fakeImagePrimaryTaskRepository{
		task:    &ImagePrimaryTask{ID: 1, PublicID: "imgp_response", Status: ImagePrimaryStatusQueued},
		created: true,
	}
	router := newImagePrimaryRouter(client, repo, true, 10*time.Millisecond, time.Millisecond)

	result := router.Route(context.Background(), ImagePrimaryRouteRequest{
		PublicID: "imgp_response", UserID: 7, APIKeyID: 9,
		Protocol: ImagePrimaryProtocolResponses, Model: "gpt-5.4", RequestHash: "hash-response",
		Submit: &ImagePrimarySubmit{ClientTaskID: "imgp_response", Payload: map[string]any{}},
	})

	require.Equal(t, ImagePrimarySuccess, result.Decision)
	require.Equal(t, 1, repo.lastTransition.ImageCount)
}
