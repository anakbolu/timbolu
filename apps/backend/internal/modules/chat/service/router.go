package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	chatdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/chat/domain"
	llmdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// RouterDeps are the dependencies of the group router.
type RouterDeps struct {
	Gateway llmdomain.Gateway
	Agents  chatdomain.AgentDirectory
	Logger  *zap.Logger
}

// GroupRouter picks the Bolu that answers a group message.
//
// A small model call decides, because "who should handle this" is a judgement
// about meaning that keywords get wrong in exactly the cases that matter. The
// call is a routing call, so its cost lands in the usage ledger under its own
// purpose and is visible in the report rather than hidden inside the answer.
//
// The decision is not allowed to fail the message: when the model is
// unavailable or answers with something that is not a candidate, a deterministic
// score picks instead and the event says so.
type GroupRouter struct {
	deps RouterDeps
}

// NewGroupRouter builds the router.
func NewGroupRouter(deps RouterDeps) *GroupRouter {
	return &GroupRouter{deps: deps}
}

// Pick implements chatdomain.Router.
func (r *GroupRouter) Pick(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation, message chatdomain.Message) (chatdomain.RouterDecision, error) {
	candidates, err := r.candidates(ctx, scope, conversation)
	if err != nil {
		return chatdomain.RouterDecision{}, err
	}
	switch len(candidates) {
	case 0:
		return chatdomain.RouterDecision{}, chatdomain.ErrNoResponder
	case 1:
		return chatdomain.RouterDecision{
			AgentID:    candidates[0].ID,
			Reason:     "hanya " + candidates[0].Name + " yang bisa menjawab",
			Source:     chatdomain.RouterSourceFallback,
			Candidates: 1,
		}, nil
	}

	question := renderBlocksForModel(message.Blocks)

	if decision, ok := r.askModel(ctx, scope, candidates, question); ok {
		return decision, nil
	}

	// The fallback is deliberately simple and explainable: the Bolu whose role
	// shares the most words with the message, and failing that the first active
	// one. It is what runs when the model is unreachable, and it never leaves a
	// group message unanswered.
	best := scoreByRole(candidates, question)
	return chatdomain.RouterDecision{
		AgentID:    best.ID,
		Reason:     fmt.Sprintf("dipilih dari perannya (%s)", best.Role),
		Source:     chatdomain.RouterSourceFallback,
		Candidates: len(candidates),
	}, nil
}

// candidates returns the Bolu of the conversation that accept new work.
func (r *GroupRouter) candidates(ctx context.Context, scope chatdomain.Scope, conversation chatdomain.Conversation) ([]chatdomain.AgentRef, error) {
	ids := make([]uuid.UUID, 0, len(conversation.Participants))
	for _, participant := range conversation.Participants {
		if participant.IsAgent() {
			ids = append(ids, participant.AgentID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}

	agents, err := r.deps.Agents.Agents(ctx, scope, ids)
	if err != nil {
		return nil, err
	}

	// A resting Bolu is not a candidate: the switch is the user's, and answering
	// would contradict it.
	active := make([]chatdomain.AgentRef, 0, len(agents))
	for _, agent := range agents {
		if r.deps.Agents.Active(agent) {
			active = append(active, agent)
		}
	}
	return active, nil
}

// askModel asks the model which Bolu should answer.
func (r *GroupRouter) askModel(ctx context.Context, scope chatdomain.Scope, candidates []chatdomain.AgentRef, question string) (chatdomain.RouterDecision, bool) {
	if r.deps.Gateway == nil {
		return chatdomain.RouterDecision{}, false
	}

	roster := make([]string, 0, len(candidates))
	for _, agent := range candidates {
		roster = append(roster, fmt.Sprintf("- %s (%s): %s", agent.Name, agent.Role, oneLine(agent.Persona)))
	}

	// The routing call runs without an agent override, so it uses the
	// workspace's first provider: the cheapest one the workspace chose.
	response, err := r.deps.Gateway.Chat(ctx, llmdomain.Scope{
		UserID:      scope.UserID,
		WorkspaceID: scope.WorkspaceID,
	}, llmdomain.ChatRequest{
		System: "Kamu router obrolan grup. Pilih satu Bolu yang paling tepat menjawab pesan terakhir. " +
			"Jawab hanya dengan namanya, tanpa penjelasan.",
		Messages: []llmdomain.Message{{
			Role: llmdomain.RoleUser,
			Text: "Bolu yang tersedia:\n" + strings.Join(roster, "\n") + "\n\nPesan terakhir:\n" + question,
		}},
		MaxTokens: 16,
		Metadata: llmdomain.RequestMetadata{
			WorkspaceID: scope.WorkspaceID,
			Purpose:     llmdomain.PurposeRouting,
		},
	})
	if err != nil {
		if r.deps.Logger != nil {
			r.deps.Logger.Warn("router fell back: the model did not answer", zap.Error(err))
		}
		return chatdomain.RouterDecision{}, false
	}

	chosen := matchCandidate(candidates, response.Text)
	if chosen == nil {
		if r.deps.Logger != nil {
			r.deps.Logger.Warn("router fell back: the model named nobody in the group",
				zap.String("answer", oneLine(response.Text)))
		}
		return chatdomain.RouterDecision{}, false
	}

	return chatdomain.RouterDecision{
		AgentID:    chosen.ID,
		Reason:     "dipilih karena paling cocok dengan pesanmu",
		Source:     chatdomain.RouterSourceModel,
		Candidates: len(candidates),
	}, true
}

// matchCandidate finds the candidate a model answer named.
//
// The comparison is on whole words rather than substrings: a model that answers
// "Biru" must not match a Bolu called "Biru Muda" by accident, and one that
// answers with a sentence still matches the name inside it.
func matchCandidate(candidates []chatdomain.AgentRef, answer string) *chatdomain.AgentRef {
	words := wordSet(answer)
	if len(words) == 0 {
		return nil
	}

	for i := range candidates {
		if words[strings.ToLower(candidates[i].Name)] {
			return &candidates[i]
		}
	}
	return nil
}

// scoreByRole is the deterministic fallback: the Bolu whose role and persona
// share the most words with the message.
func scoreByRole(candidates []chatdomain.AgentRef, question string) chatdomain.AgentRef {
	asked := wordSet(question)

	best := candidates[0]
	bestScore := -1

	for _, agent := range candidates {
		score := 0
		for word := range wordSet(agent.Role + " " + agent.Persona) {
			if asked[word] {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = agent, score
		}
	}

	return best
}

// wordSet lowercases a text and keeps its words, which is what both the match
// and the score compare.
func wordSet(text string) map[string]bool {
	words := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		word := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		return !word && r != '-' && r != '_'
	}) {
		if len(field) < 3 {
			// Short words carry no signal and match by accident.
			continue
		}
		words[field] = true
	}
	return words
}

func oneLine(text string) string {
	replaced := strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	return strings.TrimSpace(strings.Join(strings.Fields(replaced), " "))
}

// compile-time check: the router satisfies the port.
var _ chatdomain.Router = (*GroupRouter)(nil)
