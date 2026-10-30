package handler

import (
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/task/domain"
	"github.com/anakbolu/timbolu/apps/backend/internal/modules/task/service"
	"github.com/anakbolu/timbolu/apps/backend/internal/platform/response"
)

// The attention list: the work that needs a person, with the link that diagnoses it.
//
// It is one endpoint rather than three, because it is one question — "what is waiting on me?" — and
// a screen that asked for the stuck, the parked, and the failed separately would show a list whose
// sections disagreed about their own paging. What the three have in common is exactly what the
// screen needs: none of them is going to move without somebody doing something.
//
// The Temporal link is built here rather than in the browser. The deployment is the only thing that
// knows which Temporal UI it has — a compose port, a Cloud host, or none at all — so the URL is a
// server fact, and a link the frontend assembled from configuration it does not have would be a
// broken link in production and a working one on a laptop.

// attentionPayload is one task that needs a person.
type attentionPayload struct {
	service.PayloadShape
	// Attention is why it is on the list: stuck, waiting, or failed.
	Attention string `json:"attention"`
	// TemporalURL opens the task's workflow in this deployment's Temporal UI. Empty when the
	// deployment named none, or when the task never reached the engine.
	TemporalURL string `json:"temporal_url,omitempty"`
}

// Attention returns the tasks a person has to do something about.
//
// The order is the order of urgency: a task that stopped, then one that is waiting on a decision,
// then one a worker died under. It is the order a screen should read, and it is decided here rather
// than left to the client, because the reason each is on the list is a server-side fact.
func (h *Handler) Attention(c *fiber.Ctx) error {
	limit := c.QueryInt("limit", 0)

	tasks, err := h.service.History(c.UserContext(), scope(c), domain.Filter{
		// The three states are read in one call each and merged, because `status` is one value
		// and the answer to this question is a set of them. Reading each separately also means
		// the limit applies to each kind, so a workspace with fifty failures still shows its
		// parked tasks — which is the whole point of the screen.
		Status: domain.StatusFailed,
		Limit:  limit,
	})
	if err != nil {
		return h.fail(c, "list failed tasks", err)
	}

	payload := make([]attentionPayload, 0, len(tasks))
	for _, task := range tasks {
		payload = append(payload, h.attention(task, attentionFailed))
	}

	live, err := h.service.History(c.UserContext(), scope(c), domain.Filter{Live: true, Limit: limit})
	if err != nil {
		return h.fail(c, "list live tasks", err)
	}

	// A live task is on the list only when it is not going to move on its own: one that is
	// running is doing its job, and listing it would bury the ones that are not.
	waiting := []attentionPayload{}
	for _, task := range live {
		health := task.Health(h.now())
		switch health {
		case domain.HealthWaiting:
			waiting = append(waiting, h.attention(task, attentionWaiting))
		case domain.HealthStuck:
			payload = append(payload, h.attention(task, attentionStuck))
		}
	}

	// The parked tasks come after the failed ones and before nothing else: a person who has an
	// approval to make and a failure to look at should make the decision first, because a task
	// that is waiting is holding work that is otherwise done.
	payload = append(payload, waiting...)

	return response.OKWithMeta(c, "ok", payload, fiber.Map{"count": len(payload)})
}

// The three reasons a task is on the list.
const (
	attentionStuck   = "stuck"
	attentionWaiting = "waiting"
	attentionFailed  = "failed"
)

// attention renders one task for the list.
func (h *Handler) attention(task domain.Task, reason string) attentionPayload {
	return attentionPayload{
		PayloadShape: service.Payload(task, h.now()),
		Attention:    reason,
		TemporalURL:  h.temporalURL(task),
	}
}

// temporalURL builds the link that opens a task's workflow.
//
// The path is the self-hosted UI's own (`/namespaces/<namespace>/workflows/<id>/<run>`), which
// Temporal Cloud serves at the same path under its own host — so one format covers both, and the
// only deployment fact is the base. A task with no workflow never reached the engine — it is
// recorded and queued — and a link to a workflow that does not exist would be worse than no link.
func (h *Handler) temporalURL(task domain.Task) string {
	base := strings.TrimRight(strings.TrimSpace(h.temporalUI), "/")
	if base == "" || task.WorkflowID == "" {
		return ""
	}

	path := "/namespaces/" + url.PathEscape(h.temporalNamespace) + "/workflows/" + url.PathEscape(task.WorkflowID)
	if task.TemporalRunID != "" {
		// The run is part of the link: a workflow id is not unique over time, so a URL without
		// it opens whichever run the UI picks rather than the one that happened.
		path += "/" + url.PathEscape(task.TemporalRunID)
	}

	return base + path
}

// WithTemporalUI installs the deployment's Temporal UI, which is what a stuck task's link points at.
//
// It is installed after construction for the same reason the auditors are: the handler is built from
// the service interface, and the deployment's own URLs are the container's to know.
func (h *Handler) WithTemporalUI(baseURL, namespace string) *Handler {
	h.temporalUI = baseURL
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	h.temporalNamespace = namespace

	return h
}
