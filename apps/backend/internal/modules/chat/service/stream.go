package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
)

// streamBuffer is how many events a slow client may fall behind before the
// publisher is throttled.
const streamBuffer = 128

// replayChunk bounds one replay query. A client that has been away for a long
// time gets its backlog in pages rather than one huge read.
const replayChunk = 500

// Stream returns one client's view of the workspace stream: the backlog it
// missed, then everything live.
//
// The order of the two steps is what makes the stream complete. The backlog is
// read first and the subscription is made after, so an event that happens in
// between is in the backlog (it is written before it is published) and a
// duplicate is dropped by its ID rather than shown twice.
func (s *Service) Stream(ctx context.Context, scope chatdomain.Scope, afterID int64) (<-chan chatdomain.Event, error) {
	if err := requireWorkspace(scope); err != nil {
		return nil, err
	}
	if s.deps.Publisher == nil {
		return nil, fmt.Errorf("chat: the live stream is not configured")
	}

	events := make(chan chatdomain.Event, streamBuffer)

	// Subscribe before reading the backlog: the publisher's confirmation makes
	// every later publish ordered after this point, so nothing slips between the
	// two steps.
	live, err := s.deps.Publisher.Subscribe(ctx, scope.WorkspaceID)
	if err != nil {
		return nil, err
	}

	backlog, err := s.backlog(ctx, scope.WorkspaceID, afterID)
	if err != nil {
		return nil, err
	}

	go func() {
		defer close(events)

		last := afterID
		for _, event := range backlog {
			if !sendEvent(ctx, events, event) {
				return
			}
			last = event.ID
		}

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-live:
				if !ok {
					return
				}
				if event.ID <= last {
					// Already delivered from the backlog; the two paths overlap
					// by design.
					continue
				}
				if !sendEvent(ctx, events, event) {
					return
				}
				last = event.ID
			}
		}
	}()

	return events, nil
}

// backlog reads everything after an ID, in pages, so a long absence does not
// turn into one unbounded query.
func (s *Service) backlog(ctx context.Context, workspaceID uuid.UUID, afterID int64) ([]chatdomain.Event, error) {
	events := make([]chatdomain.Event, 0, replayChunk)
	cursor := afterID

	for {
		page, err := s.deps.Events.SinceEvents(ctx, workspaceID, cursor, replayChunk)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return events, nil
		}

		events = append(events, page...)
		cursor = page[len(page)-1].ID
		if len(page) < replayChunk {
			return events, nil
		}

		// A hard stop: a client asking for a backlog of a million events is
		// either a bug or an attack, and neither should hold a connection.
		if len(events) >= replayChunk*8 {
			return events, nil
		}
	}
}

// sendEvent hands one event to a client, giving up when its context ends.
func sendEvent(ctx context.Context, events chan<- chatdomain.Event, event chatdomain.Event) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}
