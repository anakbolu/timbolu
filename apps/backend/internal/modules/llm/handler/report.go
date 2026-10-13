package handler

import (
	"encoding/csv"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/response"
)

// The usage screen's read, and its export.
//
// Two endpoints rather than one, because the two consumers want different things: a screen wants
// numbers it can format, and an accountant wants a file with a header and rows that their own tool
// opens. Serving the CSV from the same read as the JSON is what keeps the two from disagreeing — a
// second query written for the export is how a spreadsheet and a dashboard start telling different
// stories about the same month.

// usageReportPayload is the report as the screen reads it.
//
// The field names are the ones the wire already uses for usage — `input_tokens`, `cost_micros` — so
// a client that renders the daily rows renders these without a second mapping. `cost_idr` is carried
// beside the micros because a screen should not have to know the unit: rupiah is what a person reads,
// micro-rupiah is what the ledger stores, and both are on the payload rather than one being derived
// in a component.
type usageReportPayload struct {
	Days int `json:"days"`
	// PeriodStartedAt is the first moment the window covers, in Asia/Jakarta. It is stated
	// rather than left to the client to recompute: the window is resolved server-side, and a
	// screen that recomputed it from `days` would disagree across a midnight.
	PeriodStartedAt string              `json:"period_started_at"`
	Totals          usageTotalsPayload  `json:"totals"`
	ByAgent         []usageAgentPayload `json:"by_agent"`
	ByTask          []usageTaskPayload  `json:"by_task"`
}

type usageTotalsPayload struct {
	Calls            int64   `json:"calls"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostMicros       int64   `json:"cost_micros"`
	CostIDR          float64 `json:"cost_idr"`
}

type usageAgentPayload struct {
	AgentID      string  `json:"agent_id"`
	AgentName    string  `json:"agent_name"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostMicros   int64   `json:"cost_micros"`
	CostIDR      float64 `json:"cost_idr"`
}

type usageTaskPayload struct {
	TaskID       string  `json:"task_id"`
	TaskTitle    string  `json:"task_title"`
	TaskStatus   string  `json:"task_status"`
	Calls        int64   `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostMicros   int64   `json:"cost_micros"`
	CostIDR      float64 `json:"cost_idr"`
}

// Report returns the period's usage, read three ways, or the same thing as a CSV.
func (h *Handler) Report(c *fiber.Ctx) error {
	days := c.QueryInt("days", 0)
	limit := c.QueryInt("limit", 0)

	if h.reporter == nil {
		return response.Error(c, fiber.StatusServiceUnavailable,
			"laporan pemakaian tidak tersedia di deployment ini", "usage_report_unavailable", nil)
	}

	report, err := h.reporter.Report(c.UserContext(), scope(c), days, limit)
	if err != nil {
		return h.fail(c, "read usage report", err)
	}

	if c.Query("format") == "csv" {
		return h.reportCSV(c, report, days)
	}

	return response.OK(c, "ok", reportPayload(report, days))
}

// reportPayload renders the report for a screen.
func reportPayload(report domain.UsageReport, days int) usageReportPayload {
	payload := usageReportPayload{
		Days:            days,
		PeriodStartedAt: report.PeriodStartedAt.Format(time.RFC3339),
		Totals: usageTotalsPayload{
			Calls:            report.Totals.Calls,
			InputTokens:      report.Totals.InputTokens,
			OutputTokens:     report.Totals.OutputTokens,
			TotalTokens:      report.Totals.Total(),
			CacheReadTokens:  report.Totals.CacheReadTokens,
			CacheWriteTokens: report.Totals.CacheWriteTokens,
			CostMicros:       report.Totals.CostMicros,
			CostIDR:          idr(report.Totals.CostMicros),
		},
		ByAgent: make([]usageAgentPayload, 0, len(report.ByAgent)),
		ByTask:  make([]usageTaskPayload, 0, len(report.ByTask)),
	}

	for _, agent := range report.ByAgent {
		payload.ByAgent = append(payload.ByAgent, usageAgentPayload{
			AgentID:      agent.AgentID.String(),
			AgentName:    agent.AgentName,
			Calls:        agent.Calls,
			InputTokens:  agent.InputTokens,
			OutputTokens: agent.OutputTokens,
			TotalTokens:  agent.Total(),
			CostMicros:   agent.CostMicros,
			CostIDR:      idr(agent.CostMicros),
		})
	}

	for _, task := range report.ByTask {
		payload.ByTask = append(payload.ByTask, usageTaskPayload{
			TaskID:       task.TaskID.String(),
			TaskTitle:    task.TaskTitle,
			TaskStatus:   task.TaskStatus,
			Calls:        task.Calls,
			InputTokens:  task.InputTokens,
			OutputTokens: task.OutputTokens,
			TotalTokens:  task.Total(),
			CostMicros:   task.CostMicros,
			CostIDR:      idr(task.CostMicros),
		})
	}

	return payload
}

// reportCSV writes the report as a spreadsheet, for internal accounting.
//
// It is one rectangular table rather than three sections, and that is the decision that makes it
// useful: every row carries the same columns and the `bagian` (section) column says which kind of row
// it is. A CSV with two-field preamble rows and nine-field data rows is a file a strict reader
// refuses — the standard library's own refuses it, with "wrong number of fields" — and one a
// spreadsheet loads as text in its first columns. One shape, one header, and a pivot table does the
// rest.
//
// The numbers are written as raw micro-rupiah *and* as rupiah with two decimals. Both, because the
// two consumers differ: a spreadsheet sums the micros without a rounding argument, and a person reads
// the rupiah. The decimal is always a dot, never a comma: the export is a machine-read file, and a
// locale-dependent separator would make the column text in half the countries it is opened in.
func (h *Handler) reportCSV(c *fiber.Ctx, report domain.UsageReport, days int) error {
	c.Set(fiber.HeaderContentType, "text/csv; charset=utf-8")
	c.Set(fiber.HeaderContentDisposition,
		`attachment; filename="pemakaian-`+time.Now().In(time.UTC).Format(time.DateOnly)+`.csv"`)

	writer := csv.NewWriter(c.Response().BodyWriter())

	// The column order is the order the questions are asked in: what it was, then how much, then
	// what it cost.
	rows := [][]string{{
		"bagian", "id", "nama", "status", "periode_mulai", "hari",
		"panggilan", "token_masuk", "token_keluar", "total_token",
		"cache_baca", "cache_tulis", "biaya_mikro_rupiah", "biaya_rupiah",
	}}

	period := report.PeriodStartedAt.Format(time.RFC3339)
	days_ := strconv.Itoa(days)

	rows = append(rows, []string{
		"total", "", "", "", period, days_,
		strconv.FormatInt(report.Totals.Calls, 10),
		strconv.FormatInt(report.Totals.InputTokens, 10),
		strconv.FormatInt(report.Totals.OutputTokens, 10),
		strconv.FormatInt(report.Totals.Total(), 10),
		strconv.FormatInt(report.Totals.CacheReadTokens, 10),
		strconv.FormatInt(report.Totals.CacheWriteTokens, 10),
		strconv.FormatInt(report.Totals.CostMicros, 10),
		money(report.Totals.CostMicros),
	})

	for _, agent := range report.ByAgent {
		rows = append(rows, []string{
			"per_bolu", agent.AgentID.String(), agent.AgentName, "", period, days_,
			strconv.FormatInt(agent.Calls, 10),
			strconv.FormatInt(agent.InputTokens, 10),
			strconv.FormatInt(agent.OutputTokens, 10),
			strconv.FormatInt(agent.Total(), 10),
			"", "",
			strconv.FormatInt(agent.CostMicros, 10),
			money(agent.CostMicros),
		})
	}

	for _, task := range report.ByTask {
		rows = append(rows, []string{
			"per_tugas", task.TaskID.String(), task.TaskTitle, task.TaskStatus, period, days_,
			strconv.FormatInt(task.Calls, 10),
			strconv.FormatInt(task.InputTokens, 10),
			strconv.FormatInt(task.OutputTokens, 10),
			strconv.FormatInt(task.Total(), 10),
			"", "",
			strconv.FormatInt(task.CostMicros, 10),
			money(task.CostMicros),
		})
	}

	if err := writer.WriteAll(rows); err != nil {
		h.log.Error("the usage export could not be written", zap.Error(err))

		return h.fail(c, "write usage csv", err)
	}

	return nil
}

// idr turns micro-rupiah into rupiah, which is what a person reads.
func idr(micros int64) float64 { return float64(micros) / 1_000_000 }

// money renders micro-rupiah as rupiah with two decimals.
//
// It is written by hand rather than with a formatter: the export is read by a spreadsheet, and a
// formatter would follow the process's locale — a comma decimal separator in some deployments would
// make the column text in the two-column countries it imported into.
func money(micros int64) string {
	whole := micros / 1_000_000
	cents := micros % 1_000_000 / 10_000
	if cents < 0 {
		cents = -cents
	}

	if whole < 0 || micros < 0 {
		return "-" + strconv.FormatInt(-whole, 10) + "." + pad2(cents)
	}

	return strconv.FormatInt(whole, 10) + "." + pad2(cents)
}

// pad2 renders a two-digit fraction part.
func pad2(value int64) string {
	if value < 10 {
		return "0" + strconv.FormatInt(value, 10)
	}

	return strconv.FormatInt(value, 10)
}
