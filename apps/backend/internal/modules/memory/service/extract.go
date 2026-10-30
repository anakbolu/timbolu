package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/memory/domain"
)

// maxCandidates bounds what one pass may write, so a model that answers with
// forty memories costs a bounded number of rows and embeddings.
const maxCandidates = 12

// minCandidateRunes is how short a proposed memory may be. "oke" and "makasih"
// are conversation, not memory, and a length floor is what catches them when the
// model ignores the instruction to skip them.
const minCandidateRunes = 12

// extractionSystem is the instruction the extractor runs under.
//
// It asks for JSON and names the two layers, because the model is the only thing
// that can tell a general workspace fact from a role-specific one. It also says
// what to ignore, which is the part that keeps a transcript from becoming a log.
const extractionSystem = `Kamu merawat memori sebuah usaha kecil. Baca transkrip percakapan dan ambil HANYA hal yang berguna di percakapan berikutnya:
- fakta tentang usaha (jam buka, harga, ongkir, alamat, kebijakan),
- preferensi pemilik (cara yang dia sukai),
- instruksi yang tahan lama ("mulai sekarang ...", "jangan ...").

Abaikan obrolan sesaat: sapaan, ucapan terima kasih, pertanyaan sekali pakai, dan hasil pekerjaan yang sudah selesai.

Balas HANYA JSON, tanpa penjelasan:
{"memories":[{"content":"satu kalimat berdiri sendiri","kind":"fact|preference|instruction","layer":"main|private"}]}

layer "main" untuk hal yang berlaku bagi seluruh workspace; layer "private" untuk hal yang hanya menyangkut cara kerja Bolu ini.
Tulis maksimal 5 memori, masing-masing di bawah 200 karakter. Kalau tidak ada yang perlu diingat, balas {"memories":[]}.`

// Extract reads a quiet conversation and writes what is worth keeping.
//
// It is the activity the extraction workflow calls. Everything it writes passes
// the same door a person's request does: the sensitive filter, an embedding, and
// then the comparison against what is already remembered, which is what keeps a
// thread read twice from producing two copies of one fact.
func (s *Service) Extract(ctx context.Context, scope domain.Scope, request domain.ExtractRequest) (domain.ExtractResult, error) {
	if scope.IsZero() {
		return domain.ExtractResult{}, fmt.Errorf("%w: workspace is required", domain.ErrInvalidInput)
	}
	if request.ConversationID == uuid.Nil {
		return domain.ExtractResult{}, fmt.Errorf("%w: conversation id is required", domain.ErrInvalidInput)
	}

	if request.AgentID != uuid.Nil {
		enabled, err := s.deps.Repository.ExtractionEnabled(ctx, scope, request.AgentID)
		if err != nil {
			return domain.ExtractResult{}, err
		}
		if !enabled {
			return domain.ExtractResult{Disabled: true}, nil
		}
	}

	utterances, err := s.read(ctx, scope, request)
	if err != nil {
		return domain.ExtractResult{}, err
	}
	if len(utterances) < 2 {
		// One message is not a conversation, and there is nothing to compare it
		// against.
		return domain.ExtractResult{}, nil
	}

	proposed, err := s.propose(ctx, scope, request, utterances)
	if err != nil {
		return domain.ExtractResult{}, err
	}

	return s.keep(ctx, scope, request, utterances, proposed)
}

// read loads the transcript.
func (s *Service) read(ctx context.Context, scope domain.Scope, request domain.ExtractRequest) ([]domain.Utterance, error) {
	if s.deps.Threads == nil {
		return nil, fmt.Errorf("%w: no conversation reader is configured", domain.ErrNoModel)
	}

	return s.deps.Threads.Recent(ctx, scope, request.ConversationID, s.limits.ExtractionMessages)
}

// propose asks the small model what is worth keeping.
func (s *Service) propose(
	ctx context.Context,
	scope domain.Scope,
	request domain.ExtractRequest,
	utterances []domain.Utterance,
) ([]domain.Extracted, error) {
	if s.deps.Model == nil {
		return nil, fmt.Errorf("%w: no model is configured", domain.ErrNoModel)
	}

	temperature := 0.0
	response, err := s.deps.Model.Chat(ctx, llmdomain.Scope{
		WorkspaceID: scope.WorkspaceID,
		AgentID:     request.AgentID,
	}, llmdomain.ChatRequest{
		System: extractionSystem,
		Messages: []llmdomain.Message{{
			Role: llmdomain.RoleUser,
			Text: transcript(utterances),
		}},
		MaxTokens:   800,
		Temperature: &temperature,
		Metadata: llmdomain.RequestMetadata{
			WorkspaceID: scope.WorkspaceID,
			AgentID:     request.AgentID,
			Purpose:     llmdomain.PurposeExtraction,
		},
	})
	if err != nil {
		return nil, err
	}

	return parseExtracted(response.Text)
}

// keep stores what survived the filter, merging with what is already remembered.
func (s *Service) keep(
	ctx context.Context,
	scope domain.Scope,
	request domain.ExtractRequest,
	utterances []domain.Utterance,
	proposed []domain.Extracted,
) (domain.ExtractResult, error) {
	result := domain.ExtractResult{}

	candidates := s.candidates(proposed, &result)
	if len(candidates) == 0 {
		return result, nil
	}

	contents := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		contents = append(contents, candidate.Content)
	}

	vectors, err := s.embed(ctx, scope, request.AgentID, uuid.Nil, contents)
	if err != nil {
		// Nothing is written when the vectors are missing: a memory a search can
		// never find is worse than a memory that arrives on the next pass.
		return result, err
	}

	source := lastUserMessage(utterances)

	for i, candidate := range candidates {
		agentID := uuid.Nil
		if candidate.Layer == domain.LayerPrivate {
			agentID = request.AgentID
		}

		nearest, err := s.deps.Repository.SearchMemories(ctx, scope, domain.SearchQuery{
			AgentID:       agentID,
			Vector:        vectors[i],
			MinSimilarity: s.limits.Similarity,
			Limit:         1,
		})
		if err != nil {
			return result, err
		}

		if len(nearest) > 0 && sameText(nearest[0].Content, candidate.Content) {
			// The same fact, said again: what a second pass over one thread must
			// not produce.
			result.Skipped++
			continue
		}

		stored, err := s.deps.Repository.CreateMemory(ctx, scope, domain.NewMemory{
			AgentID:         agentID,
			Content:         candidate.Content,
			Kind:            candidate.Kind,
			SourceMessageID: source,
			CreatedBy:       domain.SourceExtraction,
			Embedding:       vectors[i],
		})
		if err != nil {
			return result, err
		}

		if len(nearest) > 0 {
			// The old memory becomes history rather than a second opinion: two
			// versions of "ongkir Bandung" in one prompt is worse than one.
			if err := s.deps.Repository.Supersede(ctx, scope, nearest[0].ID, stored.ID); err != nil {
				return result, err
			}
			result.Merged++

			// The actor here is the workflow, not a person: nobody asked for this line, and saying
			// it was a person would make the trail lie about the one thing it is read for. The
			// source in the metadata is what says where it came from.
			s.audit(ctx, scope, domain.AuditEntry{
				Action:     domain.ActionUpdated,
				ObjectType: "memory",
				ObjectID:   stored.ID.String(),
				Metadata: map[string]any{
					"layer":      stored.Layer(),
					"kind":       stored.Kind,
					"superseded": nearest[0].ID.String(),
					"source":     "extraction",
				},
			})
			continue
		}

		result.Stored++
		s.audit(ctx, scope, domain.AuditEntry{
			Action:     domain.ActionCreated,
			ObjectType: "memory",
			ObjectID:   stored.ID.String(),
			Metadata: map[string]any{
				"layer":  stored.Layer(),
				"kind":   stored.Kind,
				"source": "extraction",
			},
		})
	}

	return result, nil
}

// candidates validates what the model proposed: known kinds, known layers, a
// length floor, no secrets, and a bounded count.
func (s *Service) candidates(proposed []domain.Extracted, result *domain.ExtractResult) []domain.Extracted {
	kept := make([]domain.Extracted, 0, len(proposed))

	for _, candidate := range proposed {
		content := oneLine(candidate.Content)
		if len([]rune(content)) < minCandidateRunes {
			result.Skipped++
			continue
		}

		if _, sensitive := Sensitive(content); sensitive {
			result.Refused++
			continue
		}

		if !knownKind(candidate.Kind) {
			candidate.Kind = domain.KindFact
		}
		if candidate.Layer != domain.LayerPrivate {
			candidate.Layer = domain.LayerMain
		}
		candidate.Content = sentence(content)

		kept = append(kept, candidate)
		if len(kept) >= maxCandidates {
			break
		}
	}

	return kept
}

// transcript renders the thread the way the extractor reads it.
func transcript(utterances []domain.Utterance) string {
	var builder strings.Builder
	builder.WriteString("Transkrip percakapan:\n")

	for _, utterance := range utterances {
		text := oneLine(utterance.Text)
		if text == "" {
			continue
		}

		speaker := "Pengguna"
		if utterance.FromAgent() {
			speaker = "Bolu " + utterance.AgentName
		}

		builder.WriteString(speaker)
		builder.WriteString(": ")
		builder.WriteString(text)
		builder.WriteString("\n")
	}

	return builder.String()
}

// parseExtracted reads the model's answer.
//
// It accepts either a bare array or the object the prompt asks for, because a
// small model occasionally drops the wrapper, and refusing that answer would
// throw away a usable extraction over punctuation.
func parseExtracted(raw string) ([]domain.Extracted, error) {
	body := stripFence(raw)

	// The wrapper is only read when the answer is an object: a bare array parsed
	// as an object would succeed with an empty list, because a JSON object with
	// none of the fields it names is still a valid object. That silent success is
	// the failure mode this ordering exists to avoid.
	if strings.HasPrefix(body, "{") {
		var wrapped struct {
			Memories []domain.Extracted `json:"memories"`
		}
		if err := json.Unmarshal([]byte(body), &wrapped); err == nil && strings.Contains(body, `"memories"`) {
			return wrapped.Memories, nil
		}
	}

	// Anything else falls back to the outermost array, which also covers an
	// answer wrapped in prose the model added anyway.
	if start := strings.Index(body, "["); start >= 0 {
		if end := strings.LastIndex(body, "]"); end > start {
			var bare []domain.Extracted
			if err := json.Unmarshal([]byte(body[start:end+1]), &bare); err == nil {
				return bare, nil
			}
		}
	}

	return nil, fmt.Errorf("%w: the extractor did not answer with JSON", domain.ErrInvalidInput)
}

// stripFence removes a markdown code fence, which small models add even when the
// instruction says not to.
func stripFence(raw string) string {
	body := strings.TrimSpace(raw)

	if !strings.HasPrefix(body, "```") {
		return body
	}

	body = strings.TrimPrefix(body, "```")
	if index := strings.Index(body, "\n"); index >= 0 {
		body = body[index+1:]
	}

	return strings.TrimSuffix(strings.TrimSpace(body), "```")
}

// sentence keeps one line and ends it with a full stop when it has none, so a
// memory reads as a statement rather than as a fragment.
func sentence(content string) string {
	if strings.HasSuffix(content, ".") || strings.HasSuffix(content, "!") || strings.HasSuffix(content, "?") {
		return content
	}
	return content + "."
}

// sameText reports whether two memories say the same thing. The comparison is
// case-insensitive and on collapsed whitespace, because the model writes the same
// fact slightly differently every time and a trailing full stop is not a new
// memory.
func sameText(left, right string) bool {
	return strings.EqualFold(strings.TrimRight(oneLine(left), "."), strings.TrimRight(oneLine(right), "."))
}

// lastUserMessage is the message a proposed memory is attributed to. The model
// reads a whole thread, so the newest thing the user said is the closest honest
// answer to "where did this come from".
func lastUserMessage(utterances []domain.Utterance) uuid.UUID {
	for i := len(utterances) - 1; i >= 0; i-- {
		if !utterances[i].FromAgent() {
			return utterances[i].MessageID
		}
	}
	return uuid.Nil
}
