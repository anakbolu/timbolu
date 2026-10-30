// Package adapter exposes the plan a Bolu keeps for itself as tools the hub can run.
//
// It is an adapter in the hub's sense and, like the order module's, it is not a connected account: a
// plan belongs to the Bolu, there is no provider, no token, and nothing outside this deployment to
// reach. `NeedsConnection` says so, and it is what the tool filter reads to offer these tools to a
// Bolu whose profile asks for them without demanding a grant.
//
// The adapter is deliberately thin: it turns a tool call's JSON into a service call and the answer
// back into JSON, and decides nothing of its own. Which plan is open, whether a tick is allowed, and
// when a plan is finished are the service's rules; a second place that decided them would be a second
// answer to the same question.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/checklist/domain"
	integrationdomain "github.com/anakbolu/timbolu/apps/backend/internal/modules/integration/domain"
)

// App is the application key this adapter answers for.
const App = "checklist"

// Tool names.
const (
	// ToolCreate writes the plan down. It is `write_internal`: it changes what is inside Bolu and
	// nothing outside it, so nobody is asked before it runs.
	ToolCreate = "checklist.create"
	// ToolCheck ticks one task or subtask.
	ToolCheck = "checklist.check"
	// ToolRemove drops one task from the plan.
	ToolRemove = "checklist.remove"
	// ToolClear throws the whole plan away, which is what frees the Bolu to write another.
	ToolClear = "checklist.clear"
	// ToolRead answers what the plan is. It is `read`: it looks, and nothing changes.
	ToolRead = "checklist.read"
)

// Service is the part of the module this adapter uses.
//
// It is declared here rather than taken as the whole service, so the adapter can say exactly what it
// needs: read the plan, write one, tick a line, drop a line, throw the plan away. Anything wider would
// let a tool reach parts of the module that are not its business.
type Service interface {
	Read(ctx context.Context, scope domain.Scope, agentID uuid.UUID) (domain.Checklist, error)
	Create(ctx context.Context, scope domain.Scope, plan domain.NewChecklist) (domain.Checklist, error)
	Check(ctx context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error)
	Remove(ctx context.Context, scope domain.Scope, agentID, itemID uuid.UUID) (domain.Checklist, error)
	Clear(ctx context.Context, scope domain.Scope, agentID uuid.UUID) error
}

// Adapter is the plan integration.
type Adapter struct {
	plans Service
}

// New builds the adapter.
func New(plans Service) *Adapter { return &Adapter{plans: plans} }

// App implements integrationdomain.Adapter.
func (a *Adapter) App() string { return App }

// NeedsToken implements integrationdomain.Credentialled: there is no token, because there is no
// account. The Bolu is the account.
func (a *Adapter) NeedsToken() bool { return false }

// NeedsConnection implements integrationdomain.Connectionless.
//
// False is the whole point of this adapter: a plan is the Bolu's own, so a call needs no connected
// account and no grant. The tool filter reads the same fact from `tool_catalog.needs_connection`, and a
// test asserts the two agree.
func (a *Adapter) NeedsConnection() bool { return false }

// Triggers implements integrationdomain.Adapter. A plan announces nothing.
func (a *Adapter) Triggers() []integrationdomain.TriggerSpec { return nil }

// Tools implements integrationdomain.Adapter.
func (a *Adapter) Tools() []integrationdomain.ToolSpec {
	return []integrationdomain.ToolSpec{
		{
			Name:  ToolCreate,
			Label: integrationdomain.LabelWriteInternal,
			Description: "Tulis rencana kerja kamu sendiri: daftar tugas, masing-masing boleh punya " +
				"sub-tugas. Pakai ini kalau pekerjaannya butuh beberapa langkah, supaya langkahnya tidak " +
				"hilang di tengah jalan. Satu Bolu hanya boleh punya satu rencana aktif; selesaikan dulu " +
				"rencana sebelumnya sebelum menulis yang baru.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"title": {
						"type": "string",
						"description": "Judul rencananya, misalnya 'Tagih pelanggan bulan ini'"
					},
					"tasks": {
						"type": "array",
						"description": "Tugas-tugasnya, urut sesuai yang kamu tulis",
						"items": {
							"type": "object",
							"properties": {
								"title": {"type": "string", "description": "Satu tugas"},
								"subtasks": {
									"type": "array",
									"description": "Langkah-langkah kecil di dalam tugas ini, kalau ada",
									"items": {"type": "string"}
								}
							},
							"required": ["title"]
						}
					}
				},
				"required": ["tasks"]
			}`),
		},
		{
			Name:  ToolCheck,
			Label: integrationdomain.LabelWriteInternal,
			Description: "Tandai satu tugas atau sub-tugas di rencanamu sudah selesai. Pakai id yang " +
				"diberikan checklist.create atau checklist.read. Tugas induk baru bisa ditandai selesai " +
				"kalau semua sub-tugasnya sudah selesai.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "id tugas atau sub-tugas yang sudah selesai"}
				},
				"required": ["id"]
			}`),
		},
		{
			Name:  ToolRemove,
			Label: integrationdomain.LabelWriteInternal,
			Description: "Hapus satu tugas dari rencana yang sedang aktif, kalau tugas itu memang tidak " +
				"perlu dikerjakan lagi. Sub-tugasnya ikut terhapus.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"id": {"type": "string", "description": "id tugas atau sub-tugas yang mau dihapus"}
				},
				"required": ["id"]
			}`),
		},
		{
			Name:  ToolClear,
			Label: integrationdomain.LabelWriteInternal,
			Description: "Buang seluruh rencana yang sedang aktif, kalau rencananya sudah tidak dipakai " +
				"lagi. Setelah ini kamu bebas menulis rencana baru. Pakai ini hanya kalau rencananya " +
				"memang salah atau batal, bukan untuk menghindari menyelesaikan pekerjaan.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name:  ToolRead,
			Label: integrationdomain.LabelRead,
			Description: "Baca rencana yang sedang aktif: tugas dan sub-tugasnya beserta id dan mana yang " +
				"sudah selesai. Pakai ini kalau kamu perlu tahu apa yang masih tersisa.",
			Schema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
	}
}

// Execute implements integrationdomain.Adapter.
func (a *Adapter) Execute(ctx context.Context, call integrationdomain.Call) (integrationdomain.Result, error) {
	scope := domain.Scope{UserID: call.Scope.UserID, WorkspaceID: call.Scope.WorkspaceID}

	switch call.Tool {
	case ToolCreate:
		return a.create(ctx, scope, call)
	case ToolCheck:
		return a.check(ctx, scope, call)
	case ToolRemove:
		return a.remove(ctx, scope, call)
	case ToolClear:
		return a.clear(ctx, scope, call)
	case ToolRead:
		return a.read(ctx, scope, call)
	default:
		return integrationdomain.Result{}, fmt.Errorf("%w: %s", integrationdomain.ErrUnknownTool, call.Tool)
	}
}

// ---------------------------------------------------------------- the tools

// createArguments is what the create tool accepts.
type createArguments struct {
	Title string `json:"title"`
	Tasks []struct {
		Title    string   `json:"title"`
		Subtasks []string `json:"subtasks"`
	} `json:"tasks"`
}

// create writes the plan down and answers with it, ids and all, so the model can tick it later.
func (a *Adapter) create(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	var args createArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return integrationdomain.Result{}, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	tasks := make([]domain.NewItem, 0, len(args.Tasks))
	for _, task := range args.Tasks {
		tasks = append(tasks, domain.NewItem{Title: task.Title, Subtasks: task.Subtasks})
	}

	plan, err := a.plans.Create(ctx, scope, domain.NewChecklist{
		AgentID: call.Scope.AgentID,
		TaskID:  call.Scope.TaskID,
		Title:   args.Title,
		Tasks:   tasks,
	})
	if err != nil {
		return integrationdomain.Result{}, err
	}

	_, total := plan.Counts()

	return a.planResult(plan, fmt.Sprintf("Rencana %q dibuat: %d tugas, %d langkah", plan.Title, len(plan.Items), total))
}

// checkArguments is what the check and remove tools accept.
type checkArguments struct {
	ID string `json:"id"`
}

// check ticks one item and answers with the plan as it now is.
func (a *Adapter) check(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	itemID, err := a.itemID(call)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	plan, err := a.plans.Check(ctx, scope, call.Scope.AgentID, itemID)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	done, total := plan.Counts()
	summary := fmt.Sprintf("Satu langkah selesai (%d/%d)", done, total)
	if !plan.Active() {
		summary = fmt.Sprintf("Rencana %q selesai: %d langkah", plan.Title, total)
	}

	return a.planResult(plan, summary)
}

// remove drops one item and answers with what is left.
func (a *Adapter) remove(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	itemID, err := a.itemID(call)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	plan, err := a.plans.Remove(ctx, scope, call.Scope.AgentID, itemID)
	if err != nil {
		return integrationdomain.Result{}, err
	}

	done, total := plan.Counts()
	summary := fmt.Sprintf("Satu tugas dihapus dari rencana (%d/%d selesai)", done, total)
	if !plan.Active() {
		summary = fmt.Sprintf("Rencana %q selesai: %d langkah", plan.Title, total)
	}

	return a.planResult(plan, summary)
}

// clear throws the plan away.
func (a *Adapter) clear(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	if err := a.plans.Clear(ctx, scope, call.Scope.AgentID); err != nil {
		return integrationdomain.Result{}, err
	}

	content, err := json.Marshal(map[string]any{
		"dibuang": true,
		"catatan": "Rencana yang aktif sudah dibuang. Kamu bebas menulis rencana baru dengan checklist.create.",
	})
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("checklist: render result: %w", err)
	}

	return integrationdomain.Result{Content: content, Summary: "Rencana aktif dibuang"}, nil
}

// read answers what the plan is.
//
// A Bolu with no plan is a successful answer rather than a failure: "what am I working on" has the
// answer "nothing", and a model that receives an error learns its tool is broken instead.
func (a *Adapter) read(ctx context.Context, scope domain.Scope, call integrationdomain.Call) (integrationdomain.Result, error) {
	plan, err := a.plans.Read(ctx, scope, call.Scope.AgentID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			content, marshalErr := json.Marshal(map[string]any{
				"rencana": nil,
				"catatan": "Belum ada rencana aktif. Tulis satu dengan checklist.create kalau pekerjaannya butuh beberapa langkah.",
			})
			if marshalErr != nil {
				return integrationdomain.Result{}, fmt.Errorf("checklist: render result: %w", marshalErr)
			}

			return integrationdomain.Result{Content: content, Summary: "Belum ada rencana aktif"}, nil
		}

		return integrationdomain.Result{}, err
	}

	return a.planResult(plan, fmt.Sprintf("Rencana %q dibaca", plan.Title))
}

// ---------------------------------------------------------------- rendering

// itemID reads the id a check or remove call names.
//
// A malformed id is refused here rather than at the database, because the model is the one that has to
// fix it and "not a uuid" is a sentence it can act on where a driver error is not.
func (a *Adapter) itemID(call integrationdomain.Call) (uuid.UUID, error) {
	var args checkArguments
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", integrationdomain.ErrSchemaViolation, err)
	}

	itemID, err := uuid.Parse(strings.TrimSpace(args.ID))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: id tugasnya harus uuid, bukan %q", integrationdomain.ErrSchemaViolation, args.ID)
	}

	return itemID, nil
}

// planResult renders a plan as the document the model reads.
//
// The ids travel with every answer, which is what makes `check` and `remove` callable without a read
// in between: a model that has just been told what the plan is can tick a line without asking again.
func (a *Adapter) planResult(plan domain.Checklist, summary string) (integrationdomain.Result, error) {
	done, total := plan.Counts()

	content, err := json.Marshal(map[string]any{
		"rencana": map[string]any{
			"id":      plan.ID.String(),
			"judul":   plan.Title,
			"status":  plan.Status,
			"selesai": done,
			"total":   total,
			"tugas":   renderItems(plan.Items),
		},
	})
	if err != nil {
		return integrationdomain.Result{}, fmt.Errorf("checklist: render result: %w", err)
	}

	return integrationdomain.Result{Content: content, Summary: summary}, nil
}

// renderItems renders the tasks, each with its subtasks.
func renderItems(items []domain.Item) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		rendered := map[string]any{
			"id":      item.ID.String(),
			"judul":   item.Title,
			"selesai": item.Done,
		}

		if len(item.Subtasks) > 0 {
			subtasks := make([]map[string]any, 0, len(item.Subtasks))
			for _, subtask := range item.Subtasks {
				subtasks = append(subtasks, map[string]any{
					"id":      subtask.ID.String(),
					"judul":   subtask.Title,
					"selesai": subtask.Done,
				})
			}

			rendered["sub_tugas"] = subtasks
		}

		out = append(out, rendered)
	}

	return out
}
