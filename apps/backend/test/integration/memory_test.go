package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	agentdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/domain"
	agentrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/repository"
	agentservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/agent/service"
	authrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/repository"
	llmdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/llm/domain"
	"github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/domain"
	memoryrepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/repository"
	memoryservice "github.com/robzlabz/employeebot/apps/backend/internal/modules/memory/service"
	workspacedomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
	workspacerepo "github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/repository"
	"github.com/robzlabz/employeebot/apps/backend/internal/platform/database"
)

// memoryFixture is one onboarded workspace with a Bolu, ready to remember.
type memoryFixture struct {
	repository *memoryrepo.Repository
	agents     *agentrepo.Repository
	pool       *database.Pool
	workspace  uuid.UUID
	userID     uuid.UUID
	agentID    uuid.UUID
	scope      domain.Scope
}

func newMemoryFixture(t *testing.T, dsn string) *memoryFixture {
	t.Helper()

	pool := poolFor(t, dsn)
	agents := agentrepo.New(pool)
	workspaces := workspacerepo.New(pool)

	user, err := authrepo.New(pool.PgxPool()).CreateUser(t.Context(), uuid.NewString()+"@example.com", "$argon2id$hash")
	require.NoError(t, err)

	result, err := workspaces.Onboard(t.Context(), user.ID, workspacedomain.OnboardRequest{
		Name:          "Toko Sinar",
		BusinessField: "Retail",
		Timezone:      "Asia/Jakarta",
	})
	require.NoError(t, err)

	agentScope := agentdomain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID}
	service := agentservice.New(agentservice.Deps{Repository: agents})

	teams, err := service.Teams(t.Context(), agentScope)
	require.NoError(t, err)
	require.NotEmpty(t, teams)

	agent, err := service.Create(t.Context(), agentScope, agentdomain.CreateRequest{
		TeamID: teams[0].ID,
		Name:   "Oren",
	})
	require.NoError(t, err)

	return &memoryFixture{
		repository: memoryrepo.New(pool),
		agents:     agents,
		pool:       pool,
		workspace:  result.Workspace.ID,
		userID:     user.ID,
		agentID:    agent.ID,
		scope:      domain.Scope{UserID: user.ID, WorkspaceID: result.Workspace.ID},
	}
}

// countInScope reads a count inside the tenant scope, because every memory table
// carries Row Level Security: a count read outside a scope returns zero whatever
// the table holds, which would make an assertion pass for the wrong reason.
func countInScope(t *testing.T, fixture *memoryFixture, query string, args ...any) int {
	t.Helper()

	var count int
	require.NoError(t, fixture.pool.InScopeRead(t.Context(), database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), query, args...).Scan(&count)
	}))

	return count
}

// vector builds a unit-ish vector whose first component is the value, so two
// vectors can be told apart by distance without a real embedding model.
func vector(value float32) []float32 {
	out := make([]float32, llmdomain.EmbeddingDimensions)
	out[0] = value
	out[1] = 1 - value
	return out
}

// TestMemoryVectorSearchNeverCrossesAWorkspace is the E10.1 gate: two
// workspaces, one query, and only the caller's own rows come back. It is proven
// against Postgres rather than against a fake, because the thing under test is
// the policy and the filter, and a fake would only re-state the test.
func TestMemoryVectorSearchNeverCrossesAWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	mine := newMemoryFixture(t, dsn)
	theirs := newMemoryFixture(t, dsn)
	ctx := t.Context()

	// The same fact, written by both workspaces, with the same vector. A search
	// that forgot its filter would return both.
	theirMemory, err := theirs.repository.CreateMemory(ctx, theirs.scope, domain.NewMemory{
		Content:   "Nomor rekening toko sebelah",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	myMemory, err := mine.repository.CreateMemory(ctx, mine.scope, domain.NewMemory{
		Content:   "Nomor rekening toko kami",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	found, err := mine.repository.SearchMemories(ctx, mine.scope, domain.SearchQuery{
		Vector: vector(0.5),
		Limit:  10,
	})
	require.NoError(t, err)

	require.Len(t, found, 1)
	require.Equal(t, myMemory.ID, found[0].ID)
	require.NotEqual(t, theirMemory.ID, found[0].ID)
	require.Equal(t, "Nomor rekening toko kami", found[0].Content)

	// And the other direction, so the assertion is not one query's accident.
	found, err = theirs.repository.SearchMemories(ctx, theirs.scope, domain.SearchQuery{
		Vector: vector(0.5),
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, theirMemory.ID, found[0].ID)

	// Knowledge is filtered by the same rule.
	theirChunk, err := theirs.repository.CreateKnowledge(ctx, theirs.scope, []domain.NewKnowledge{
		{Source: "rahasia.txt", Content: "Ongkir toko sebelah 9 ribu"},
	})
	require.NoError(t, err)
	require.NoError(t, theirs.repository.EmbedKnowledge(ctx, theirs.scope, []uuid.UUID{theirChunk[0].ID}, [][]float32{vector(0.5)}))

	chunks, err := mine.repository.SearchKnowledge(ctx, mine.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Empty(t, chunks, "another workspace's document must never be returned")
}

// TestPrivateMemoryDoesNotLeakBetweenBolu is the E10.3 gate's second half: the
// main memory is shared, a private one is not.
func TestPrivateMemoryDoesNotLeakBetweenBolu(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	// A second Bolu in the same workspace.
	service := agentservice.New(agentservice.Deps{Repository: fixture.agents})
	teams, err := service.Teams(ctx, agentdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace})
	require.NoError(t, err)

	other, err := service.Create(ctx, agentdomain.Scope{UserID: fixture.userID, WorkspaceID: fixture.workspace},
		agentdomain.CreateRequest{TeamID: teams[0].ID, Name: "Biru"})
	require.NoError(t, err)

	mine, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		AgentID:   fixture.agentID,
		Content:   "Kamu selalu menulis tanggal di tagihan",
		Kind:      domain.KindInstruction,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	main, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		Content:   "Jam buka 08.00-21.00",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	// The other Bolu's private search finds nothing.
	found, err := fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{
		AgentID: other.ID,
		Vector:  vector(0.5),
		Limit:   10,
	})
	require.NoError(t, err)
	require.Empty(t, found, "a private memory belongs to one Bolu")

	// The main layer is readable by the workspace and holds only the main row.
	mainRows, err := fixture.repository.ListMemories(ctx, fixture.scope, domain.MemoryFilter{Main: true})
	require.NoError(t, err)
	require.Len(t, mainRows, 1)
	require.Equal(t, main.ID, mainRows[0].ID)

	// The Bolu's own search finds its own memory.
	found, err = fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{
		AgentID: fixture.agentID,
		Vector:  vector(0.5),
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, mine.ID, found[0].ID)
}

// TestDeletingAMemoryRemovesItFromTheSearch is the E10.7 gate's second half:
// after a delete, a similarity search no longer finds the row. It is a real
// delete, and it takes its own retrieval record with it so nothing is left
// naming something that is gone.
func TestDeletingAMemoryRemovesItFromTheSearch(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	stored, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		Content:   "Ongkir Bandung 15 ribu",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	require.NoError(t, fixture.repository.RecordRetrieval(ctx, fixture.scope, domain.Retrieval{
		TaskID:     uuid.New(),
		AgentID:    fixture.agentID,
		MemoryID:   stored.ID,
		Similarity: 0.95,
	}))

	// It is findable, so the delete is what makes it unfindable rather than the
	// search having never worked.
	found, err := fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Len(t, found, 1)

	require.NoError(t, fixture.repository.DeleteMemory(ctx, fixture.scope, stored.ID))

	found, err = fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Empty(t, found, "a deleted memory must not be found by the search")

	// The row is gone from the table, not flagged: there is no soft delete.
	require.Zero(t, countInScope(t, fixture, "SELECT count(*) FROM memories WHERE id = $1", stored.ID),
		"the delete is permanent")
	require.Zero(t, countInScope(t, fixture, "SELECT count(*) FROM retrieval_usages WHERE memory_id = $1", stored.ID),
		"and the record naming it goes too")
}

// TestSupersededMemoryStaysAsHistory is the E10.5 gate: a corrected fact leaves
// one active memory and one readable old version.
func TestSupersededMemoryStaysAsHistory(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	older, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		Content:   "Ongkir Bandung 15 ribu",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceExtraction,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	newer, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		Content:   "Ongkir Bandung 18 ribu",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceExtraction,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	require.NoError(t, fixture.repository.Supersede(ctx, fixture.scope, older.ID, newer.ID))

	// The search sees one memory: two versions of the same fact in one prompt is
	// worse than one.
	found, err := fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, "Ongkir Bandung 18 ribu", found[0].Content)

	// The screen's default list also shows one...
	active, err := fixture.repository.ListMemories(ctx, fixture.scope, domain.MemoryFilter{})
	require.NoError(t, err)
	require.Len(t, active, 1)

	// ...and the history is available when it is asked for.
	history, err := fixture.repository.ListMemories(ctx, fixture.scope, domain.MemoryFilter{IncludeSuperseded: true})
	require.NoError(t, err)
	require.Len(t, history, 2)

	superseded, err := fixture.repository.Memory(ctx, fixture.scope, older.ID)
	require.NoError(t, err)
	require.Equal(t, newer.ID, superseded.SupersededBy, "the old version points at what replaced it")
}

// TestChunksSettleToReadyOrFailed is the E10.1 gate's other half: an upload is
// monitorable, and a failed chunk never reaches a prompt.
func TestChunksSettleToReadyOrFailed(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	pending, err := fixture.repository.CreateKnowledge(ctx, fixture.scope, []domain.NewKnowledge{
		{Source: "katalog.pdf", ChunkIndex: 0, Content: "Ongkir Bandung 15 ribu"},
		{Source: "katalog.pdf", ChunkIndex: 1, Content: "Ongkir Surabaya 25 ribu"},
	})
	require.NoError(t, err)
	require.Len(t, pending, 2)

	// A pending chunk exists so the upload is visible, and it is not searchable:
	// a vector-less row would match every query at distance zero.
	for _, item := range pending {
		require.Equal(t, domain.StatusPending, item.Status)
	}

	found, err := fixture.repository.SearchKnowledge(ctx, fixture.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Empty(t, found, "a chunk with no vector must not be returned")

	// One chunk embeds, the other fails.
	require.NoError(t, fixture.repository.EmbedKnowledge(ctx, fixture.scope,
		[]uuid.UUID{pending[0].ID}, [][]float32{vector(0.5)}))
	require.NoError(t, fixture.repository.FailKnowledge(ctx, fixture.scope, []uuid.UUID{pending[1].ID}))

	ready, err := fixture.repository.Knowledge(ctx, fixture.scope, pending[0].ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusReady, ready.Status)

	failed, err := fixture.repository.Knowledge(ctx, fixture.scope, pending[1].ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusFailed, failed.Status)

	// Only the ready chunk is searchable, and the failed one is still visible on
	// the screen so a person can see the upload did not work.
	found, err = fixture.repository.SearchKnowledge(ctx, fixture.scope, domain.SearchQuery{Vector: vector(0.5), Limit: 10})
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.Equal(t, pending[0].ID, found[0].ID)

	all, err := fixture.repository.ListKnowledge(ctx, fixture.scope, domain.KnowledgeFilter{})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

// TestMemoryExtractionSwitchIsReadableAndWritable is the E10.5 switch: it
// defaults on, the memory module reads it, and it can be turned off.
func TestMemoryExtractionSwitch(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	enabled, err := fixture.repository.ExtractionEnabled(ctx, fixture.scope, fixture.agentID)
	require.NoError(t, err)
	require.True(t, enabled, "extraction is on by default: a memory never written is a feature nobody notices")

	updated, err := fixture.agents.SetMemoryExtraction(ctx, agentdomain.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, fixture.agentID, false)
	require.NoError(t, err)
	require.False(t, updated.MemoryExtraction)

	enabled, err = fixture.repository.ExtractionEnabled(ctx, fixture.scope, fixture.agentID)
	require.NoError(t, err)
	require.False(t, enabled)

	agents, err := fixture.repository.Agents(ctx, fixture.scope)
	require.NoError(t, err)
	require.Len(t, agents, 1)
	require.Equal(t, "Oren", agents[0].Name)
	require.False(t, agents[0].ExtractionEnabled)
}

// TestMemoryFailsWhenAnIdIsFromAnotherWorkspace: the statement refuses a Bolu
// that is not in the caller's workspace, which is what stops a private memory
// from being written into somebody else's layer.
func TestMemoryFailsWhenAnIdIsFromAnotherWorkspace(t *testing.T) {
	dsn := appDatabase(t)
	mine := newMemoryFixture(t, dsn)
	theirs := newMemoryFixture(t, dsn)
	ctx := t.Context()

	_, err := mine.repository.CreateMemory(ctx, mine.scope, domain.NewMemory{
		AgentID:   theirs.agentID,
		Content:   "Kamu selalu menulis tanggal di tagihan",
		Kind:      domain.KindInstruction,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.ErrorIs(t, err, domain.ErrInvalidInput, "the Bolu is not in this workspace")

	// A write with no agent at all is the main layer, which is allowed.
	_, err = mine.repository.CreateMemory(ctx, mine.scope, domain.NewMemory{
		Content:   "Jam buka 08.00-21.00",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)
}

// TestRetrievalIsRecordedForBothKinds is the E10.2 measurement: which chunks a
// round was given is answerable afterwards, for a memory and for a document.
func TestRetrievalIsRecordedForBothKinds(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	memory, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		Content:   "Kamu selalu menulis tanggal di tagihan",
		Kind:      domain.KindInstruction,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	chunks, err := fixture.repository.CreateKnowledge(ctx, fixture.scope, []domain.NewKnowledge{
		{Source: "katalog.pdf", Content: "Ongkir Bandung 15 ribu"},
	})
	require.NoError(t, err)
	require.NoError(t, fixture.repository.EmbedKnowledge(ctx, fixture.scope,
		[]uuid.UUID{chunks[0].ID}, [][]float32{vector(0.5)}))

	taskID := uuid.New()
	require.NoError(t, fixture.repository.RecordRetrieval(ctx, fixture.scope, domain.Retrieval{
		TaskID: taskID, AgentID: fixture.agentID, MemoryID: memory.ID, Similarity: 0.93,
	}))
	require.NoError(t, fixture.repository.RecordRetrieval(ctx, fixture.scope, domain.Retrieval{
		TaskID: taskID, AgentID: fixture.agentID, KnowledgeID: chunks[0].ID, Similarity: 0.91,
	}))

	require.Equal(t, 2, countInScope(t, fixture,
		"SELECT count(*) FROM retrieval_usages WHERE task_id = $1", taskID))

	// A row that names both a memory and a document is not a retrieval, which is
	// why the constraint is there. The write runs in scope so the policy admits
	// it and the check constraint is what refuses it.
	insertErr := fixture.pool.InScope(ctx, database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO retrieval_usages (workspace_id, memory_id, knowledge_item_id) VALUES ($1, $2, $3)",
			fixture.workspace, memory.ID, chunks[0].ID)
		return err
	})
	require.Error(t, insertErr, "a retrieval names exactly one chunk")
}

// TestThePipelineEndToEnd drives the whole module against Postgres: a message
// asks for something to be remembered, the confirmation names it, the next
// round is given it, and forgetting it removes it from the search.
func TestThePipelineEndToEnd(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	repository := fixture.repository
	embedder := &fixtureEmbedder{}
	service := memoryservice.New(memoryservice.Deps{
		Repository: repository,
		Embedder:   embedder,
	})

	// E10.4: a direct request is stored and confirmed.
	result, err := service.Remember(ctx, fixture.scope, domain.RememberRequest{
		Text:    "ingat ya, jam buka 08.00-21.00",
		AgentID: fixture.agentID,
		// A request from chat carries the message it came from; a request with no
		// conversation behind it carries none, which is why the column is nullable.
		MessageID: uuid.Nil,
	})
	require.NoError(t, err)
	require.True(t, result.Handled)
	require.False(t, result.Refused)
	require.NotNil(t, result.Stored)
	require.Contains(t, result.Confirmation, "jam buka 08.00-21.00",
		"the confirmation must name exactly what was stored")

	// E10.3: the next round is given it, in the stable half.
	recall, err := service.Recall(ctx, fixture.scope, domain.RecallRequest{
		AgentID: fixture.agentID,
		TaskID:  uuid.New(),
		Query:   "jam buka",
	})
	require.NoError(t, err)
	require.Contains(t, recall.Stable, "jam buka 08.00-21.00")

	// E10.7: forgetting it removes it from the search as well as the table.
	require.NoError(t, service.Forget(ctx, fixture.scope, result.Stored.ID))

	found, err := repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{
		Vector: embedder.last,
		Limit:  10,
	})
	require.NoError(t, err)
	require.Empty(t, found)
}

// fixtureEmbedder answers a deterministic vector, so the pipeline can run
// against Postgres without a provider.
type fixtureEmbedder struct {
	last []float32
}

func (f *fixtureEmbedder) Embed(_ context.Context, _ llmdomain.Scope, req llmdomain.EmbedRequest) (llmdomain.EmbedResponse, error) {
	vectors := make([][]float32, 0, len(req.Inputs))
	for _, input := range req.Inputs {
		// The same text always embeds to the same vector, and different text to a
		// different one, which is what makes the search answerable.
		value := float32(len(input)) / 100
		vectors = append(vectors, vector(value))
	}
	f.last = vectors[len(vectors)-1]
	return llmdomain.EmbedResponse{Vectors: vectors, Model: "fixture-embed"}, nil
}

// TestSchemaAddsTheMemoryColumns guards the migration itself: the switch column,
// the retrieval table, and its policy.
func TestSchemaAddsTheMemoryColumns(t *testing.T) {
	dsn := appDatabase(t)
	ctx := t.Context()

	pool, err := database.New(ctx, database.Config{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// A workspace, because the insert below has to reach the constraint check
	// rather than selecting no rows at all.
	fixture := newMemoryFixture(t, dsn)

	var hasSwitch bool
	require.NoError(t, pool.PgxPool().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns
		  WHERE table_name = 'agents' AND column_name = 'memory_extraction_enabled')`).Scan(&hasSwitch))
	require.True(t, hasSwitch)

	var rls bool
	require.NoError(t, pool.PgxPool().QueryRow(ctx,
		`SELECT relrowsecurity FROM pg_class WHERE relname = 'retrieval_usages'`).Scan(&rls))
	require.True(t, rls, "the retrieval record is tenant data like every other row")

	// The one-chunk constraint is what makes "which chunk did this answer see"
	// answerable: a row naming both, or neither, is not a retrieval.
	insertErr := fixture.pool.InScope(ctx, database.Scope{
		UserID:      fixture.scope.UserID,
		WorkspaceID: fixture.scope.WorkspaceID,
	}, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO retrieval_usages (workspace_id) VALUES ($1)", fixture.workspace)
		return err
	})
	require.Error(t, insertErr, "a retrieval must name exactly one chunk")
}

// TestAMemoryFromAnotherWorkspaceCannotBeRead closes the loop on the policies:
// an id from another tenant answers not-found rather than the row.
func TestAMemoryFromAnotherWorkspaceCannotBeRead(t *testing.T) {
	dsn := appDatabase(t)
	mine := newMemoryFixture(t, dsn)
	theirs := newMemoryFixture(t, dsn)
	ctx := t.Context()

	theirMemory, err := theirs.repository.CreateMemory(ctx, theirs.scope, domain.NewMemory{
		Content:   "Nomor rekening toko sebelah",
		Kind:      domain.KindFact,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.5),
	})
	require.NoError(t, err)

	_, err = mine.repository.Memory(ctx, mine.scope, theirMemory.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)

	// And a delete of somebody else's memory changes nothing there.
	require.ErrorIs(t, mine.repository.DeleteMemory(ctx, mine.scope, theirMemory.ID), domain.ErrNotFound)

	still, err := theirs.repository.Memory(ctx, theirs.scope, theirMemory.ID)
	require.NoError(t, err)
	require.Equal(t, theirMemory.Content, still.Content)
}

// TestUpdateMovesALayerAndReembeds is the E10.6 gate at the storage level: a
// move lands, a rewrite stores the new vector, and the old version is untouched.
func TestUpdateMovesALayerAndReembeds(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	stored, err := fixture.repository.CreateMemory(ctx, fixture.scope, domain.NewMemory{
		AgentID:   fixture.agentID,
		Content:   "Kamu selalu menulis tanggal di tagihan",
		Kind:      domain.KindInstruction,
		CreatedBy: domain.SourceUser,
		Embedding: vector(0.2),
	})
	require.NoError(t, err)

	moved, err := fixture.repository.UpdateMemory(ctx, fixture.scope, stored.ID, domain.MemoryChange{MoveMain: true})
	require.NoError(t, err)
	require.Equal(t, domain.LayerMain, moved.Layer(), "a move to the main layer clears the agent")

	// It is now found by the main-layer search, with the new vector.
	content := "Jam buka 08.00-21.00"
	updated, err := fixture.repository.UpdateMemory(ctx, fixture.scope, stored.ID, domain.MemoryChange{
		Content:   &content,
		Embedding: vector(0.9),
	})
	require.NoError(t, err)
	require.Equal(t, content, updated.Content)

	found, err := fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{
		Vector:        vector(0.9),
		MinSimilarity: 0.99,
		Limit:         10,
	})
	require.NoError(t, err)
	require.Len(t, found, 1, "the stored vector is the new text's")

	// And the old vector does not answer it.
	found, err = fixture.repository.SearchMemories(ctx, fixture.scope, domain.SearchQuery{
		Vector:        vector(0.2),
		MinSimilarity: 0.99,
		Limit:         10,
	})
	require.NoError(t, err)
	require.Empty(t, found)
}

// TestIngestAgainstPostgres is the E10.1 pipeline end to end: a document is
// chunked, embedded, and stored ready, and a search finds the chunk.
func TestIngestAgainstPostgres(t *testing.T) {
	dsn := appDatabase(t)
	fixture := newMemoryFixture(t, dsn)
	ctx := t.Context()

	embedder := &fixtureEmbedder{}
	service := memoryservice.New(memoryservice.Deps{
		Repository: fixture.repository,
		Embedder:   embedder,
		Limits:     domain.Limits{ChunkRunes: 60, ChunkOverlapRunes: 10},
	})

	document := strings.Join([]string{
		"Katalog ongkos kirim.",
		"Bandung 15 ribu untuk paket di bawah satu kilogram.",
		"Surabaya 25 ribu untuk paket di bawah satu kilogram.",
		"Medan 40 ribu dan hanya dikirim Senin dan Kamis.",
	}, "\n\n")

	stored, err := service.Ingest(ctx, fixture.scope, domain.IngestRequest{
		Source: "ongkir.txt",
		Text:   document,
	})
	require.NoError(t, err)
	require.NotEmpty(t, stored)

	for _, item := range stored {
		require.Equal(t, domain.StatusReady, item.Status, "every chunk is embedded before the upload is reported done")
	}

	rows, err := fixture.repository.ListKnowledge(ctx, fixture.scope, domain.KnowledgeFilter{})
	require.NoError(t, err)
	require.Len(t, rows, len(stored))
	for i, item := range rows {
		require.Equal(t, "ongkir.txt", item.Source)
		require.Equal(t, i, item.ChunkIndex, "the chunks keep their order")
	}

	found, err := fixture.repository.SearchKnowledge(ctx, fixture.scope, domain.SearchQuery{
		Vector: embedder.last,
		Limit:  5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, found)
}

// TestRepositoryNeedsAPool: every method reports the missing pool rather than
// dereferencing nil, which is what lets the API run without a database.
func TestRepositoryNeedsAPool(t *testing.T) {
	repository := memoryrepo.New(nil)
	ctx := context.Background()
	scope := domain.Scope{WorkspaceID: uuid.New()}

	_, err := repository.CreateMemory(ctx, scope, domain.NewMemory{Content: "x", Embedding: vector(0.5)})
	require.Error(t, err)

	_, err = repository.ListMemories(ctx, scope, domain.MemoryFilter{})
	require.Error(t, err)

	_, err = repository.SearchMemories(ctx, scope, domain.SearchQuery{Vector: vector(0.5)})
	require.Error(t, err)

	require.Error(t, repository.DeleteMemory(ctx, scope, uuid.New()))
}
