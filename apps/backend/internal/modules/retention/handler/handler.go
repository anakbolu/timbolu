// Package handler exposes the erasure endpoints: a workspace owner asking for their
// data to be removed, and reading back the record of what was removed.
//
// Two endpoints and no more, and the shape is deliberate. There is no "schedule an
// erasure" and no "cancel" — the right it implements is a *request* whose answer is
// either "done" or "here is why not", and a queue in between would be a deployment
// keeping data after being asked not to. There is also no way to erase somebody
// else's workspace: the route reads the active workspace from the request, so the
// tenant middleware is the authority rather than a parameter a caller could edit.
package handler

import (
	"errors"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/retention/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/middleware"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// Service is the half of the retention module this handler calls.
//
// It is the module's own port rather than a hand-written copy: a method added to the erasure service
// without being declared here is a route that cannot use it, and the alias makes that a compile error
// at the definition rather than a call the handler cannot make.
type Service = domain.ErasureService

// Handler serves the erasure endpoints.
type Handler struct {
	service Service
	log     *zap.Logger
}

// New builds the handler.
func New(service Service, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// OwnerRoutes registers the endpoints a workspace owner may call. The container
// applies the tenant middleware and an owner check to this group; the person is
// taken from the session rather than from the body, because who asked is part of
// the record.
func OwnerRoutes(group fiber.Router, h *Handler) {
	group.Post("/workspaces/current/erasure", h.Erase)
	group.Get("/workspaces/current/erasure", h.Show)
}

// Erase removes the active workspace's data and answers with the record.
//
// The response is the evidence, and it is returned in the same call that does the
// work rather than being fetched afterwards: the workspace no longer exists when
// this returns, so there is nothing left to address a later read to.
func (h *Handler) Erase(c *fiber.Ctx) error {
	workspaceID := middleware.WorkspaceID(c)
	id, err := uuid.Parse(workspaceID)
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "workspace id is not a uuid", "invalid_input", nil)
	}

	result, err := h.service.Erase(c.UserContext(), domain.ErasureRequest{
		WorkspaceID: id,
		RequestedBy: middleware.CurrentUserID(c),
	})
	if err != nil {
		return h.fail(c, "erase workspace", err)
	}

	return response.OK(c, "data workspace dihapus", fiber.Map{
		"workspace_id": id.String(),
		"objects":      result.Objects,
		"rows":         result.Rows,
	})
}

// Show returns the workspace's erasure record, when there is one.
//
// It exists for the case the POST cannot cover: a deployment where the request was
// made and the caller kept a copy of the id, and a person who has to answer "was this
// erased". A workspace whose data is gone answers `erasure_not_found` here — the
// active workspace no longer resolves, so the tenant middleware stops the request
// before this runs, which is the honest outcome rather than a 404 from the handler.
func (h *Handler) Show(c *fiber.Ctx) error {
	id, err := uuid.Parse(middleware.WorkspaceID(c))
	if err != nil {
		return response.Error(c, fiber.StatusBadRequest, "workspace id is not a uuid", "invalid_input", nil)
	}

	erasure, err := h.service.FindErasure(c.UserContext(), id)
	if err != nil {
		return h.fail(c, "read erasure", err)
	}

	return response.OK(c, "ok", erasurePayload(erasure))
}

// erasurePayload is the record as a person reads it.
func erasurePayload(erasure domain.Erasure) fiber.Map {
	return fiber.Map{
		"workspace_id":       erasure.WorkspaceID.String(),
		"workspace_name":     erasure.WorkspaceName,
		"requested_by":       erasure.RequestedBy.String(),
		"requested_by_email": erasure.RequestedByMail,
		"requested_at":       erasure.RequestedAt,
		"completed_at":       erasure.CompletedAt,
		"objects_deleted":    erasure.Objects,
		"rows_deleted":       erasure.Rows,
		"complete":           erasure.Done(),
	}
}

// fail maps a service error onto a response.
func (h *Handler) fail(c *fiber.Ctx, operation string, err error) error {
	switch {
	case errors.Is(err, domain.ErrErasureNotFound):
		return response.Error(c, fiber.StatusNotFound,
			"belum ada permintaan hapus data yang selesai untuk workspace ini",
			"erasure_not_found", nil)
	case errors.Is(err, domain.ErrNoRepository), errors.Is(err, domain.ErrInvalidInput):
		return response.Error(c, fiber.StatusServiceUnavailable,
			"penghapusan data tidak bisa dijalankan di deployment ini: jalur hapus belum lengkap",
			"erasure_unavailable", nil)
	default:
		h.log.Error("erasure failed", zap.String("operation", operation), zap.Error(err))

		return response.Error(c, fiber.StatusInternalServerError,
			"penghapusan data gagal dan tidak ada yang setengah dihapus: coba lagi atau hubungi kami",
			"erasure_failed", nil)
	}
}
