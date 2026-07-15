package admin

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) GetImportSnapshot(c *gin.Context) {
	accountID, ok := parseImportSnapshotAccountID(c)
	if !ok {
		return
	}
	if h.accountImportSnapshotService == nil {
		response.Error(c, http.StatusServiceUnavailable, "account import snapshot service is unavailable")
		return
	}
	view, err := h.accountImportSnapshotService.GetMasked(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	response.Success(c, view)
}

func (h *AccountHandler) RevealImportSnapshot(c *gin.Context) {
	accountID, ok := parseImportSnapshotAccountID(c)
	if !ok {
		return
	}
	if h.accountImportSnapshotService == nil {
		response.Error(c, http.StatusServiceUnavailable, "account import snapshot service is unavailable")
		return
	}
	view, err := h.accountImportSnapshotService.Reveal(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	adminID := int64(0)
	if subject, exists := middleware.GetAuthSubjectFromContext(c); exists {
		adminID = subject.UserID
	}
	slog.Info("account import snapshot revealed",
		"audit", true,
		"action", "account_import_snapshot_reveal",
		"admin_id", adminID,
		"account_id", accountID,
		"source_ip", c.ClientIP(),
		"revealed_at", time.Now().UTC(),
	)
	response.Success(c, view)
}

func parseImportSnapshotAccountID(c *gin.Context) (int64, bool) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || accountID <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	return accountID, true
}
