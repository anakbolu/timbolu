package workflow

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/task/domain"
)

// SystemPrompt is the instruction a Bolu answers a task under.
//
// It is assembled inside the workflow rather than in an activity because it is a
// pure function of the task's own data: putting it behind an activity would add a
// round trip and a database dependency for string concatenation, and a workflow
// that reads its own input is still deterministic.
func SystemPrompt(loaded Context, input TaskInput) string {
	agent := loaded.Agent

	parts := []string{
		"Kamu " + agent.Name + ", Bolu " + agent.Role + " di aplikasi Bolu.",
	}
	if agent.Persona != "" {
		parts = append(parts, agent.Persona)
	}
	if agent.Tone != "" {
		parts = append(parts, "Gaya bahasa: "+agent.Tone+".")
	}

	parts = append(parts,
		"Jawab dalam bahasa Indonesia, ringkas dan konkret.",
		"Kamu bekerja sampai tugas ini selesai: panggil alat bila perlu data, lalu simpulkan hasilnya.",
	)

	if len(loaded.Tools) > 0 {
		parts = append(parts,
			"Alat yang butuh persetujuan manusia akan berhenti menunggu keputusan; jangan mengulanginya.",
			"Kalau butuh pekerjaan yang bukan keahlianmu, pakai alat handoff ke Bolu lain.")
	}
	if loaded.Depth > 0 {
		parts = append(parts, "Tugas ini bagian dari tugas lain; serahkan hasilnya sebagai data yang bisa dipakai.")
	}

	return strings.Join(parts, "\n")
}

// Messages renders the conversation the task answers.
//
// The task's own prompt is appended as the last user turn, because the workflow
// may run long after the message that started it and the stored history is what
// the model should see.
func Messages(loaded Context, input TaskInput) []domain.Message {
	messages := make([]domain.Message, 0, len(loaded.History)+1)
	last := ""

	for _, message := range loaded.History {
		text := strings.TrimSpace(domain.RenderBlocks(message.Blocks))
		if text == "" {
			// The placeholder the task's own answer fills is empty when the
			// round starts, so it is skipped rather than sent as a blank turn.
			continue
		}
		messages = append(messages, textTurn(text, message.AgentID, message.UserID))
		last = text
	}

	// The prompt is the task's own instruction. For a task opened from chat it
	// is the message the user just wrote, which the history already carries:
	// appending it again would send the same words twice and read as the user
	// repeating themselves.
	prompt := strings.TrimSpace(input.Prompt)
	if prompt != "" && prompt != last {
		messages = append(messages, textTurn(prompt, uuid.Nil, uuid.Nil))
	}

	return messages
}

// textTurn builds one turn whose body is a real text block, which is the shape
// the activity renders back into the model's message.
func textTurn(text string, agentID, userID uuid.UUID) domain.Message {
	body, err := json.Marshal(map[string]any{"type": "text", "markdown": text})
	if err != nil {
		body = []byte(`{"type":"text","markdown":""}`)
	}

	return domain.Message{
		AgentID: agentID,
		UserID:  userID,
		Blocks:  []domain.Block{{Type: "text", Body: body}},
	}
}

// TaskEvent builds one event for the activity stream. The activities publish
// through it rather than through the service, because they run out of process.
func TaskEvent(scope domain.Scope, task domain.Task, eventType string, payload []byte) domain.Event {
	return domain.Event{
		WorkspaceID:    scope.WorkspaceID,
		Type:           eventType,
		ActorAgentID:   task.AgentID,
		ConversationID: task.ConversationID,
		TaskID:         task.ID,
		Payload:        payload,
	}
}

// marshalQuiet encodes a value for a step's payload. A value that will not encode
// becomes an empty document rather than failing the round: a step whose payload
// is unusable is worth less than the step itself.
func marshalQuiet(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// handoffTarget reads the Bolu a handoff names.
func handoffTarget(arguments []byte) string {
	var payload struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Agent)
}

// handoffInstructions reads what the handoff asks for.
func handoffInstructions(arguments []byte) string {
	var payload struct {
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return string(arguments)
	}
	return strings.TrimSpace(payload.Instructions)
}

// mustUUID parses an id. An unparsable id is the zero value, which the activity
// then reports as an invalid input rather than panicking in the workflow.
func mustUUID(value string) uuid.UUID {
	id, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return uuid.Nil
	}
	return id
}

// AnswerBlocks renders a task's answer as the blocks the conversation stores.
//
// A task that produced no prose still writes a block: an answer of nothing would
// leave the placeholder message empty, which reads as a Bolu that said nothing
// rather than one that failed. A failed task writes the reason instead.
func AnswerBlocks(answer, reason string) []domain.Block {
	text := strings.TrimSpace(answer)
	if text == "" {
		text = strings.TrimSpace(reason)
	}
	if text == "" {
		return nil
	}

	body, err := json.Marshal(map[string]any{"type": "text", "markdown": text})
	if err != nil {
		return nil
	}

	return []domain.Block{{Type: "text", Body: body}}
}
