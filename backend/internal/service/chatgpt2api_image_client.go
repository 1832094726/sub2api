package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	chatGPT2APIResponseLimit = 128 << 20
	chatGPT2APIErrorLimit    = 8 << 10
)

var ErrImagePrimaryTaskNotFound = errors.New("image primary task not found")

type ChatGPT2APIImageClientConfig struct {
	BaseURL       string
	APIKey        string
	Model         string
	ModelResolver func(context.Context) string
	HTTPClient    *http.Client
}

type ChatGPT2APIImageClient struct {
	baseURL       *url.URL
	apiKey        string
	model         string
	modelResolver func(context.Context) string
	httpClient    *http.Client
}

func NewChatGPT2APIImageClient(cfg ChatGPT2APIImageClientConfig) (*ChatGPT2APIImageClient, error) {
	baseURL, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, fmt.Errorf("invalid chatgpt2api base URL")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("invalid chatgpt2api base URL scheme")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("chatgpt2api API key is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &ChatGPT2APIImageClient{
		baseURL:       baseURL,
		apiKey:        strings.TrimSpace(cfg.APIKey),
		model:         strings.TrimSpace(cfg.Model),
		modelResolver: cfg.ModelResolver,
		httpClient:    httpClient,
	}, nil
}

func (c *ChatGPT2APIImageClient) SubmitImages(ctx context.Context, submit *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	payload, err := submitPayload(submit)
	if err != nil {
		return nil, err
	}
	payload["client_task_id"] = submit.ClientTaskID
	payload["background"] = true
	model := c.model
	if c.modelResolver != nil {
		model = strings.TrimSpace(c.modelResolver(ctx))
	}
	if model != "" {
		payload["model"] = model
	}
	return c.postJSON(ctx, "/v1/images/generations", payload)
}

func (c *ChatGPT2APIImageClient) SubmitEdits(ctx context.Context, submit *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	if submit == nil || len(submit.Body) == 0 || strings.TrimSpace(submit.ContentType) == "" {
		return nil, errors.New("image edit body and content type are required")
	}
	return c.do(ctx, http.MethodPost, "/api/image-tasks/edits", submit.ContentType, submit.Body)
}

func (c *ChatGPT2APIImageClient) SubmitResponses(ctx context.Context, submit *ImagePrimarySubmit) (*ImagePrimarySnapshot, error) {
	payload, err := submitPayload(submit)
	if err != nil {
		return nil, err
	}
	payload["client_task_id"] = submit.ClientTaskID
	return c.postJSON(ctx, "/api/image-tasks/responses", payload)
}

func (c *ChatGPT2APIImageClient) GetTask(ctx context.Context, taskID string) (*ImagePrimarySnapshot, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("task ID is required")
	}
	path := "/api/image-tasks?ids=" + url.QueryEscape(taskID)
	var page struct {
		Items []ImagePrimarySnapshot `json:"items"`
	}
	if err := c.doInto(ctx, http.MethodGet, path, "", nil, &page); err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, ErrImagePrimaryTaskNotFound
	}
	snapshot := &page.Items[0]
	if snapshot.Mode == "response" {
		var eventsPage struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor int               `json:"next_cursor"`
		}
		eventsPath := "/api/image-tasks/" + url.PathEscape(taskID) + "/events?after=0"
		if err := c.doInto(ctx, http.MethodGet, eventsPath, "", nil, &eventsPage); err != nil {
			return nil, err
		}
		snapshot.Events = eventsPage.Events
	}
	return snapshot, nil
}

func submitPayload(submit *ImagePrimarySubmit) (map[string]any, error) {
	if submit == nil || strings.TrimSpace(submit.ClientTaskID) == "" {
		return nil, errors.New("client task ID is required")
	}
	payload := make(map[string]any, len(submit.Payload)+2)
	for key, value := range submit.Payload {
		payload[key] = value
	}
	return payload, nil
}

func (c *ChatGPT2APIImageClient) postJSON(ctx context.Context, path string, payload map[string]any) (*ImagePrimarySnapshot, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode chatgpt2api request: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, "application/json", body)
}

func (c *ChatGPT2APIImageClient) do(ctx context.Context, method, path, contentType string, body []byte) (*ImagePrimarySnapshot, error) {
	var snapshot ImagePrimarySnapshot
	if err := c.doInto(ctx, method, path, contentType, body, &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (c *ChatGPT2APIImageClient) doInto(ctx context.Context, method, path, contentType string, body []byte, target any) error {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path, RawQuery: rawQuery(path)})
	endpoint.Path = strings.SplitN(path, "?", 2)[0]
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create chatgpt2api request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("chatgpt2api request failed: %s", c.redact(err.Error()))
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, chatGPT2APIResponseLimit+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read chatgpt2api response: %w", err)
	}
	if len(responseBody) > chatGPT2APIResponseLimit {
		return errors.New("chatgpt2api response exceeds size limit")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusNotFound {
			return ErrImagePrimaryTaskNotFound
		}
		detail := responseBody
		if len(detail) > chatGPT2APIErrorLimit {
			detail = detail[:chatGPT2APIErrorLimit]
		}
		return fmt.Errorf("chatgpt2api returned HTTP %d: %s", resp.StatusCode, c.redact(strings.TrimSpace(string(detail))))
	}
	if err := json.Unmarshal(responseBody, target); err != nil {
		return fmt.Errorf("decode chatgpt2api response: %w", err)
	}
	return nil
}

func rawQuery(path string) string {
	parts := strings.SplitN(path, "?", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return ""
}

func (c *ChatGPT2APIImageClient) redact(value string) string {
	if c.apiKey == "" {
		return value
	}
	return strings.ReplaceAll(value, c.apiKey, "[REDACTED]")
}
