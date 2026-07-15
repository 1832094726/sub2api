package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type imageModelSettingRepoStub struct {
	values map[string]string
}

func (s *imageModelSettingRepoStub) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *imageModelSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	return "", ErrSettingNotFound
}

func (s *imageModelSettingRepoStub) Set(context.Context, string, string) error {
	panic("unexpected Set call")
}

func (s *imageModelSettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *imageModelSettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *imageModelSettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *imageModelSettingRepoStub) Delete(context.Context, string) error {
	panic("unexpected Delete call")
}

func TestNormalizeChatGPT2APIImageModel(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty uses codex default", input: "", want: "codex-gpt-image-2"},
		{name: "codex accepted", input: " codex-gpt-image-2 ", want: "codex-gpt-image-2"},
		{name: "gpt image accepted", input: "gpt-image-2", want: "gpt-image-2"},
		{name: "unsupported rejected", input: "auto", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeChatGPT2APIImageModel(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestGetChatGPT2APIImageModel_PersistedSettingOverridesEnvironment(t *testing.T) {
	repo := &imageModelSettingRepoStub{values: map[string]string{
		SettingKeyChatGPT2APIImageModel: "gpt-image-2",
	}}
	svc := NewSettingService(repo, &config.Config{
		ChatGPT2APIImage: config.ChatGPT2APIImageConfig{Model: "codex-gpt-image-2"},
	})

	require.Equal(t, "gpt-image-2", svc.GetChatGPT2APIImageModel(context.Background()))
}

func TestGetChatGPT2APIImageModel_FallsBackToEnvironmentAndDefault(t *testing.T) {
	repo := &imageModelSettingRepoStub{values: map[string]string{}}

	withEnvironment := NewSettingService(repo, &config.Config{
		ChatGPT2APIImage: config.ChatGPT2APIImageConfig{Model: "gpt-image-2"},
	})
	require.Equal(t, "gpt-image-2", withEnvironment.GetChatGPT2APIImageModel(context.Background()))

	withDefault := NewSettingService(repo, &config.Config{})
	require.Equal(t, "codex-gpt-image-2", withDefault.GetChatGPT2APIImageModel(context.Background()))
}
