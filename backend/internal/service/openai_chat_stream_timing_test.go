package service

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestChatStreamTiming(t *testing.T) {
	for _, tc := range []struct {
		name            string
		text, failWrite bool
	}{
		{"text", true, false}, {"empty", false, false}, {"disconnected", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
			c.Header("X-Request-ID", "gateway-1")
			c.Header("X-Client-Request-ID", "billing-1")
			if tc.failWrite {
				c.Writer = &openAIChatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
			}
			r, w := io.Pipe()
			defer r.Close()
			go func() {
				defer w.Close()
				fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-6-astra\"}}\n\n")
				time.Sleep(40 * time.Millisecond)
				fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\n")
				if tc.text {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
				}
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			}()
			resp := &http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"upstream-1"}}, Body: r}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			_, err := svc.handleChatStreamingResponse(resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, "gpt-6-astra", "gpt-6-astra", "gpt-6-astra", time.Now(), 100000)
			require.NoError(t, err)
			entries := logs.FilterMessage("openai.chat_stream.timing").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, "gateway-1", fields["gateway_request_id"])
			require.Equal(t, "billing-1", fields["client_request_id"])
			require.Equal(t, "upstream-1", fields["upstream_request_id"])
			if tc.text {
				require.GreaterOrEqual(t, fields["first_text_ms"].(int64)-fields["first_event_ms"].(int64), int64(30))
				if tc.failWrite {
					require.Nil(t, fields["first_text_flush_ms"], "failed writes must not count as flushed text")
				} else {
					require.GreaterOrEqual(t, fields["first_text_flush_ms"].(int64), fields["first_text_ms"].(int64))
					require.Contains(t, rec.Body.String(), "hello")
				}
			} else {
				require.Nil(t, fields["first_text_ms"], "metadata and reasoning are not answer text")
				require.Nil(t, fields["first_text_flush_ms"])
			}
		})
	}
}
