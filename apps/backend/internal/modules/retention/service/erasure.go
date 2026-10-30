package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/retention/domain"
)

// Erase removes a workspace's data on request.
//
// The order is the design and it is the opposite of the retention pass's, for a reason worth
// stating: **the objects go first and the rows second**. If the workspace row goes first, every
// attachment row cascades with it and the storage keys are gone — the bytes are then unreachable by
// any query, in a bucket nothing points at, and the request has silently failed to erase the copy of
// the customer's file that matters most. So the bytes are removed while the keys that name them
// still exist.
//
// A failure at either step keeps the record with its reason, and the request is *not* retried
// automatically: an erasure that half-succeeded is a state a person has to look at, and a background
// retry would be the deployment deciding on its own to try again. What the record gives is the one
// thing needed to finish the job by hand — which prefix and which workspace.
func (s *Service) Erase(ctx context.Context, request domain.ErasureRequest) (domain.ErasureResult, error) {
	var result domain.ErasureResult

	if s.deps.Erasures == nil {
		return result, domain.ErrNoRepository
	}
	if request.WorkspaceID == uuid.Nil {
		return result, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if s.deps.Workspaces == nil || s.deps.Objects == nil {
		// Without both, an erasure would delete half of what the workspace held and report
		// success. Refusing is the only honest answer: the caller is told the deployment cannot
		// complete the request rather than being told it did.
		return result, fmt.Errorf("%w: the erasure path is not configured", domain.ErrInvalidInput)
	}

	// The names are read while the rows that hold them are still there: an erasure record that
	// says a uuid was deleted cannot be shown to the person who asked for their data back.
	name, err := s.deps.Workspaces.WorkspaceName(ctx, request.WorkspaceID)
	if err != nil {
		return result, fmt.Errorf("retention: read the workspace name: %w", err)
	}
	email, err := s.deps.Workspaces.RequesterEmail(ctx, request.RequestedBy)
	if err != nil {
		// An address that cannot be read is not a reason to refuse the erasure: the person's
		// right is to have their data removed, and the record can say who asked by id.
		s.warn("could not read the requester's address for the erasure record",
			zap.String("workspace", request.WorkspaceID.String()), zap.Error(err))
		email = ""
	}

	recordID, err := s.deps.Erasures.Start(ctx, request, name, email)
	if err != nil {
		return result, err
	}

	// Objects first, while the keys that name them still exist.
	objects, err := s.deps.Objects.EraseObjects(ctx, request.WorkspaceID)
	result.Objects = objects
	if err != nil {
		s.finishErasure(ctx, recordID, result, err)

		return result, err
	}

	rows, err := s.deps.Workspaces.EraseWorkspace(ctx, request.WorkspaceID)
	result.Rows = rows
	if err != nil {
		s.finishErasure(ctx, recordID, result, err)

		return result, err
	}

	s.finishErasure(ctx, recordID, result, nil)

	return result, nil
}

// Erasures returns the newest erasures, which is what an auditor reads.
func (s *Service) Erasures(ctx context.Context, limit int) ([]domain.Erasure, error) {
	if s.deps.Erasures == nil {
		return nil, domain.ErrNoRepository
	}

	return s.deps.Erasures.List(ctx, limit)
}

// FindErasure answers whether one workspace's data was erased, which is the question its owner has.
func (s *Service) FindErasure(ctx context.Context, workspaceID uuid.UUID) (domain.Erasure, error) {
	if s.deps.Erasures == nil {
		return domain.Erasure{}, domain.ErrNoRepository
	}

	return s.deps.Erasures.Find(ctx, workspaceID)
}

// finishErasure closes the record, and reports a failure it could not write.
func (s *Service) finishErasure(ctx context.Context, id int64, result domain.ErasureResult, failure error) {
	if err := s.deps.Erasures.Finish(ctx, id, result, failure); err != nil {
		s.warn("could not close the erasure record", zap.Error(err))
	}
}
