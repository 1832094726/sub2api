package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthTerminalDoesNotWaitForConnectionClose(t *testing.T) {
	for _, mode := range []string{"sync", "async", "passthrough"} {
		for _, terminal := range []string{"response.completed", "response.done"} {
			t.Run(mode+"/"+terminal, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				cfg := &config.Config{}
				if mode == "async" {
					cfg.Gateway.StreamKeepaliveInterval = 10
				}
				svc := &OpenAIGatewayService{cfg: cfg}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: "oauth"}
				payload := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
					"event: " + terminal + "\ndata: {\"type\":\"" + terminal + "\",\"response\":{\"id\":\"resp_terminal\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"
				wrote := make(chan error, 1)
				go func() { _, err := io.WriteString(writer, payload); wrote <- err }()
				type outcome struct {
					usage *OpenAIUsage
					err   error
				}
				done := make(chan outcome, 1)
				go func() {
					if mode == "passthrough" {
						r, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
						if r == nil {
							done <- outcome{err: err}
							return
						}
						done <- outcome{r.usage, err}
					} else {
						r, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-6-astra", "gpt-6-astra")
						if r == nil {
							done <- outcome{err: err}
							return
						}
						done <- outcome{r.usage, err}
					}
				}()
				select {
				case r := <-done:
					require.NoError(t, r.err)
					require.NotNil(t, r.usage)
					require.Equal(t, 3, r.usage.InputTokens)
					require.Equal(t, 2, r.usage.OutputTokens)
					require.Contains(t, rec.Body.String(), terminal)
					require.True(t, rec.Flushed)
				case <-time.After(2 * time.Second):
					_ = reader.Close()
					_ = writer.Close()
					<-done
					t.Fatal("complete terminal frame must finish without upstream EOF")
				}
				<-wrote
			})
		}
	}
}
