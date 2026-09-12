//go:build unit

package middleware

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCyberUserBlockAcrossAPIKeysAndProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, google := range []bool{false, true} {
		for _, blocked := range []bool{false, true} {
			until := time.Now().Add(-time.Second)
			if blocked {
				until = time.Now().Add(time.Hour)
			}
			repo := &stubApiKeyRepo{getByKey: func(_ context.Context, key string) (*service.APIKey, error) {
				return &service.APIKey{ID: 1, UserID: 42, Key: key, Status: service.StatusActive, User: &service.User{ID: 42, Status: service.StatusActive, CyberBlockedUntil: &until}}, nil
			}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, cfg)
			r := gin.New()
			if google {
				r.Use(APIKeyAuthGoogle(svc, cfg))
			} else {
				r.Use(gin.HandlerFunc(NewAPIKeyAuthMiddleware(svc, nil, cfg)))
			}
			r.GET("/t", func(c *gin.Context) { c.Status(http.StatusOK) })
			for _, key := range []string{"key-one", "key-two"} {
				w := httptest.NewRecorder()
				req := httptest.NewRequest("GET", "/t", nil)
				req.Header.Set("Authorization", "Bearer "+key)
				r.ServeHTTP(w, req)
				want := 200
				if blocked {
					want = 403
					require.NotEmpty(t, w.Header().Get("Retry-After"))
				}
				require.Equal(t, want, w.Code, w.Body.String())
			}
		}
	}
}
