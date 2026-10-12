package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
)

// Remember stores what a message asked to be remembered and answers with a
// confirmation that names it.
//
// The answer is written here rather than by the model so the confirmation and
// the row cannot disagree: the sentence the user reads is built from the text
// that was actually stored. A message that is not a request is left alone, and
// the ordinary reply handles it.
func (s *Service) Remember(ctx context.Context, scope domain.Scope, request domain.RememberRequest) (domain.RememberResult, error) {
	if scope.IsZero() {
		return domain.RememberResult{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}

	content, ok := RememberIntent(request.Text)
	if !ok {
		return domain.RememberResult{Handled: false}, nil
	}

	if len([]rune(content)) > s.limits.MaxContentRunes {
		return domain.RememberResult{
			Handled:      true,
			Refused:      true,
			Confirmation: "Itu terlalu panjang untuk kuingat. Ringkas jadi satu atau dua kalimat ya, lalu tulis lagi.",
		}, nil
	}

	if reason, sensitive := Sensitive(content); sensitive {
		return domain.RememberResult{
			Handled:      true,
			Refused:      true,
			Confirmation: sensitiveSentence(reason),
		}, nil
	}

	// The kind is read from the whole message, because the marker itself carries
	// the instruction: "mulai sekarang" is what makes the rest an instruction.
	kind := ClassifyKind(strings.ToLower(request.Text))

	agentID := request.AgentID
	if ClassifyLayer(content) == domain.LayerMain {
		agentID = uuid.Nil
	}

	vectors, err := s.embed(ctx, scope, agentID, uuid.Nil, []string{content})
	if err != nil {
		if s.deps.Logger != nil {
			s.deps.Logger.Warn("a memory request could not be embedded", zap.Error(err))
		}
		return domain.RememberResult{
			Handled:      true,
			Confirmation: "Maaf, aku belum bisa mengingatnya sekarang: penyimpanan memori sedang tidak tersedia. Coba lagi sebentar lagi ya.",
		}, nil
	}

	stored, err := s.deps.Repository.CreateMemory(ctx, scope, domain.NewMemory{
		AgentID:         agentID,
		Content:         content,
		Kind:            kind,
		SourceMessageID: request.MessageID,
		CreatedBy:       domain.SourceUser,
		Embedding:       vectors[0],
	})
	if err != nil {
		return domain.RememberResult{}, err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:     domain.ActionCreated,
		ObjectType: "memory",
		ObjectID:   stored.ID.String(),
		Metadata: map[string]any{
			"layer":  stored.Layer(),
			"kind":   stored.Kind,
			"source": "message",
		},
	})

	return domain.RememberResult{
		Handled:      true,
		Stored:       &stored,
		Confirmation: confirmationFor(stored),
	}, nil
}

// confirmationFor names what was stored and where it lives, which is what makes
// the answer checkable against the memory screen.
func confirmationFor(memory domain.Memory) string {
	where := "di memori utama workspace"
	if memory.Layer() == domain.LayerPrivate {
		where = "di memori pribadiku"
	}

	return fmt.Sprintf("Siap, aku ingat %s: \"%s\"", where, oneLine(memory.Content))
}
