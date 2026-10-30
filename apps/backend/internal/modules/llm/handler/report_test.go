package handler

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/anakbolu/timbolu/apps/backend/internal/modules/llm/domain"
)

// stubReporter answers with a scripted report and records what it was asked.
type stubReporter struct {
	report domain.UsageReport
	days   int
	limit  int
	calls  int
}

func (s *stubReporter) Report(_ context.Context, _ domain.Scope, days, limit int) (domain.UsageReport, error) {
	s.calls++
	s.days = days
	s.limit = limit

	return s.report, nil
}

// sampleReport is one period with both breakdowns.
func sampleReport() domain.UsageReport {
	agent := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	task := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	return domain.UsageReport{
		PeriodStartedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.FixedZone("WIB", 7*3600)),
		Totals: domain.UsageTotals{
			Calls: 12, InputTokens: 3400, OutputTokens: 900,
			CacheReadTokens: 200, CacheWriteTokens: 100, CostMicros: 4_250_000,
		},
		ByAgent: []domain.UsageByAgent{{
			AgentID: agent, AgentName: "Oren", Calls: 9,
			InputTokens: 3000, OutputTokens: 800, CostMicros: 4_100_000,
		}},
		ByTask: []domain.UsageByTask{{
			TaskID: task, TaskTitle: "Rekap pesanan", TaskStatus: "succeeded",
			Calls: 4, InputTokens: 1500, OutputTokens: 400, CostMicros: 1_800_000,
		}},
	}
}

// reportApp builds an app with the two report routes and a request identity.
func reportApp(t *testing.T, reporter domain.UsageReporter) *fiber.App {
	t.Helper()

	handler := New(nil, zap.NewNop())
	if reporter != nil {
		handler = handler.WithReporter(reporter)
	}

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(func(c *fiber.Ctx) error {
		c.Locals("workspace_id", "33333333-3333-3333-3333-333333333333")
		return c.Next()
	})
	group := app.Group("/api")
	group.Get("/llm/usage/report", handler.Report)

	return app
}

// TestTheReportCarriesBothBreakdownsAndTheRupiahFigure is what the screen reads: the totals, the two
// breakdowns, and a rupiah figure beside the micros so a component does not have to know the unit.
func TestTheReportCarriesBothBreakdownsAndTheRupiahFigure(t *testing.T) {
	reporter := &stubReporter{report: sampleReport()}
	app := reportApp(t, reporter)

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report?days=7&limit=25", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, 200, response.StatusCode)

	var payload struct {
		Data usageReportPayload `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&payload))

	require.Equal(t, 7, payload.Data.Days)
	require.Equal(t, "2026-05-01T00:00:00+07:00", payload.Data.PeriodStartedAt)

	require.Equal(t, int64(3400), payload.Data.Totals.InputTokens)
	require.Equal(t, int64(4300), payload.Data.Totals.TotalTokens)
	require.Equal(t, 4.25, payload.Data.Totals.CostIDR, "the rupiah figure is on the payload")

	require.Len(t, payload.Data.ByAgent, 1)
	require.Equal(t, "Oren", payload.Data.ByAgent[0].AgentName)
	require.Equal(t, 4.1, payload.Data.ByAgent[0].CostIDR)

	require.Len(t, payload.Data.ByTask, 1)
	require.Equal(t, "Rekap pesanan", payload.Data.ByTask[0].TaskTitle)

	require.Equal(t, 7, reporter.days, "the window the screen asked for")
	require.Equal(t, 25, reporter.limit)
}

// TestTheReportDefaultsItsWindowAndPage: a client that names neither gets the module's own answer
// rather than a zero-day window that would read nothing.
func TestTheReportDefaultsItsWindowAndPage(t *testing.T) {
	reporter := &stubReporter{report: sampleReport()}
	app := reportApp(t, reporter)

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, 200, response.StatusCode)

	require.Zero(t, reporter.days, "the service resolves the default, not the handler")
	require.Zero(t, reporter.limit)
}

// TestTheExportIsAFileAnAccountantOpens: a CSV with a header, the three sections separated, and
// numbers that do not depend on the process's locale — a comma decimal separator would make the
// column text in half the countries it is imported into.
func TestTheExportIsAFileAnAccountantOpens(t *testing.T) {
	reporter := &stubReporter{report: sampleReport()}
	app := reportApp(t, reporter)

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report?days=30&format=csv", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	require.Equal(t, 200, response.StatusCode)

	require.Contains(t, response.Header.Get("Content-Type"), "text/csv")
	require.Contains(t, response.Header.Get("Content-Disposition"), "attachment; filename=")

	rows, err := csv.NewReader(response.Body).ReadAll()
	require.NoError(t, err)

	// One rectangular table: every row carries the same columns, and `bagian` says which kind
	// of row it is. A reader that inferred its width from a shorter preamble would refuse the
	// file — the standard library's own does — and a spreadsheet would load the first columns as
	// text.
	require.Equal(t, []string{"bagian", "id", "nama", "status", "periode_mulai", "hari",
		"panggilan", "token_masuk", "token_keluar", "total_token",
		"cache_baca", "cache_tulis", "biaya_mikro_rupiah", "biaya_rupiah"}, rows[0])

	sections := []string{}
	for _, row := range rows[1:] {
		require.Len(t, row, len(rows[0]), "every row carries every column")
		sections = append(sections, row[0])
	}
	require.Equal(t, []string{"total", "per_bolu", "per_tugas"}, sections)

	// The total row carries both units, and the rupiah is a dot decimal rather than a comma.
	total := rowWithFirstCell(t, rows, "total")
	require.Equal(t, "12", total[6], "the calls")
	require.Equal(t, "3400", total[7], "input tokens")
	require.Equal(t, "4300", total[9], "total tokens")
	require.Equal(t, "4250000", total[12], "micro-rupiah, which a spreadsheet sums without rounding")
	require.Equal(t, "4.25", total[13], "and rupiah with two decimals")
	require.Equal(t, "2026-05-01T00:00:00+07:00", total[4], "the window is stated on every row")

	// And the per-Bolu row says which bot it was, by name rather than only by id.
	agent := rowWithFirstCell(t, rows, "per_bolu")
	require.Equal(t, "11111111-1111-1111-1111-111111111111", agent[1])
	require.Equal(t, "Oren", agent[2])
	require.Equal(t, "4.10", agent[13])

	// The per-task row carries the title and the status, which is what a person reconciles
	// against what they remember asking for.
	task := rowWithFirstCell(t, rows, "per_tugas")
	require.Equal(t, "Rekap pesanan", task[2])
	require.Equal(t, "succeeded", task[3])
}

// rowWithFirstCell finds one row by its first cell.
func rowWithFirstCell(t *testing.T, rows [][]string, first string) []string {
	t.Helper()

	for _, row := range rows {
		if len(row) > 0 && row[0] == first {
			return row
		}
	}

	t.Fatalf("no row starting with %q", first)

	return nil
}

// TestAnUnwiredReporterAnswers503: a deployment without the reporting port says so, rather than
// answering an empty report that looks like a workspace that spent nothing.
func TestAnUnwiredReporterAnswers503(t *testing.T) {
	app := reportApp(t, nil)

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	require.Equal(t, 503, response.StatusCode)

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "usage_report_unavailable")
}

// TestTheMoneyFormattingIsSaidRatherThanDerived: a component reads rupiah, and the conversion is
// asserted here so a screen cannot disagree with the CSV about the same month.
func TestTheMoneyFormattingIsSaidRatherThanDerived(t *testing.T) {
	require.Equal(t, "0.00", money(0))
	require.Equal(t, "1.00", money(1_000_000))
	require.Equal(t, "4.25", money(4_250_000))
	require.Equal(t, "0.05", money(50_000))
	require.Equal(t, "-1.50", money(-1_500_000), "a credit is a negative, not a positive")
	require.Equal(t, 4.25, idr(4_250_000))
}

// TestTheReportRouteIsGuardedLikeTheRestOfTheModule: it is a read of a workspace's spend, so it sits
// behind the tenant middleware rather than beside the public health routes.
func TestTheReportRouteIsGuardedLikeTheRestOfTheModule(t *testing.T) {
	// The middleware the container installs on the read group is what guards it; what this test
	// asserts is that the route is registered on that group rather than on the public prefix.
	handler := New(nil, zap.NewNop())
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	group := app.Group("/api")
	Routes(group, handler)

	registered := []string{}
	for _, route := range app.GetRoutes() {
		registered = append(registered, route.Method+" "+route.Path)
	}

	require.Contains(t, registered, "GET /api/llm/usage/report")
	require.NotContains(t, strings.Join(registered, "\n"), "GET /llm/usage/report\n",
		"the route lives under the API prefix, where the guards are")
}

// TestTheReportRendersThePublicShapeWithoutLeakingTheLedger: the payload carries counts and money
// and no row of the ledger, which is what keeps a screen from becoming a second way to read usage.
func TestTheReportRendersThePublicShapeWithoutLeakingTheLedger(t *testing.T) {
	reporter := &stubReporter{report: sampleReport()}
	app := reportApp(t, reporter)

	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/llm/usage/report", nil), -1)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	require.NotContains(t, string(body), "usage_ledger")
	require.NotContains(t, string(body), "provider", "the per-provider split stays in the daily view")
}
