package integration

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// These tests drive the operator surface over a real Postgres: the migration
// that adds the role, the default a new account gets, the guard that keeps a
// plain user out, and the two rules about demotion.
//
// The bootstrap is part of what is tested rather than worked around: the first
// admin is made by a statement on the database, because no screen grants the
// role — and a test that could promote itself through the API would be proving
// the wrong thing.

// promoteByHand makes an account a platform admin the way the migration
// documents it: one statement, by somebody with access to the database.
func promoteByHand(t *testing.T, dsn, email string) {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	defer pool.Close()

	tag, err := pool.Exec(t.Context(), "UPDATE users SET role = 'admin' WHERE lower(email) = lower($1)", email)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "the account to promote must exist")
}

// roleOf reads an account's role straight from the table, so a test can assert
// what was written rather than what a response said.
func roleOf(t *testing.T, dsn, email string) string {
	t.Helper()

	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	defer pool.Close()

	var role string
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT role FROM users WHERE lower(email) = lower($1)", email).Scan(&role))
	return role
}

// newAdminAPI is the running API over a migrated database, with the account
// table reachable so the bootstrap statement can be run against the same
// database the API reads.
type adminAPI struct {
	*identityAPI
	dsn string
}

func newAdminAPI(t *testing.T) *adminAPI {
	t.Helper()

	dsn := migratedDatabase(t)
	return &adminAPI{identityAPI: newIdentityAPI(t, dsn), dsn: dsn}
}

// rows returns the "data" array of a successful envelope. The list endpoints
// answer with an array rather than an object, which is the one shape `data`
// cannot read.
func rows(t *testing.T, payload map[string]any) []any {
	t.Helper()

	require.NotNil(t, payload, "the response had no JSON body")
	value, ok := payload["data"].([]any)
	require.True(t, ok, "the response has no data array: %v", payload)
	return value
}

// signUp registers an account and signs it in.
//
// No verification step: the harness builds the container with the deployment's
// own default, and the accounts below only have to exist and be able to call the
// API. What the platform role does is the subject here, not the email flow.
func (a *adminAPI) signUp(t *testing.T, email, password string) string {
	t.Helper()

	status, _, body := a.call(t, fiber.MethodPost, "/api/auth/register",
		`{"email":"`+email+`","password":"`+password+`"}`, nil)
	require.Equal(t, http.StatusCreated, status, body)

	access, _ := a.login(t, email, password)
	return access
}

// TestANewAccountIsAUserAndTheRoleTravelsOnTheProfile is the migration's own
// promise: every account that exists when it runs, and every account created
// afterwards, is a plain user — and the screen learns that from /auth/me rather
// than from a second call.
func TestANewAccountIsAUserAndTheRoleTravelsOnTheProfile(t *testing.T) {
	api := newAdminAPI(t)

	const email = "plain@example.com"
	access := api.signUp(t, email, "A-good-password1")

	require.Equal(t, "user", roleOf(t, api.dsn, email), "the column default is what a registration produces")

	status, payload, body := api.call(t, fiber.MethodGet, "/api/auth/me", "", bearer(access))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "user", data(t, payload)["role"])
}

// TestAPlainUserCannotReachTheOperatorSurface is the guard where a client meets
// it: the surface answers 403, and it does so without a workspace — a platform
// admin administers the deployment, so no active workspace is asked for.
func TestAPlainUserCannotReachTheOperatorSurface(t *testing.T) {
	api := newAdminAPI(t)
	access := api.signUp(t, "plain@example.com", "A-good-password1")

	for _, path := range []string{"/api/admin/overview", "/api/admin/users"} {
		status, payload, body := api.call(t, fiber.MethodGet, path, "", bearer(access))
		require.Equal(t, http.StatusForbidden, status, body)
		require.Equal(t, "forbidden", payload["error"].(map[string]any)["code"])
	}

	// And a request with no session is refused before the role is even read.
	status, _, _ := api.call(t, fiber.MethodGet, "/api/admin/overview", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}

// TestAnAdminReadsTheAccountsAndChangesARole is the surface's happy path: the
// overview counts the deployment, the list names every account, and a promotion
// is written to the table.
func TestAnAdminReadsTheAccountsAndChangesARole(t *testing.T) {
	api := newAdminAPI(t)

	adminEmail := "admin@example.com"
	memberEmail := "member@example.com"
	adminAccess := api.signUp(t, adminEmail, "A-good-password1")
	api.signUp(t, memberEmail, "A-good-password1")
	promoteByHand(t, api.dsn, adminEmail)

	// The overview counts every account, and the admin it was promoted to is one
	// of them.
	status, payload, body := api.call(t, fiber.MethodGet, "/api/admin/overview", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	stats := data(t, payload)
	require.Equal(t, float64(2), stats["total"])
	require.Equal(t, float64(1), stats["admins"])
	require.Equal(t, float64(1), stats["users"])

	// The list names both, and each row carries what an operator reads about it.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/admin/users", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	accounts := rows(t, payload)
	require.Len(t, accounts, 2)

	byEmail := map[string]map[string]any{}
	for _, entry := range accounts {
		row := entry.(map[string]any)
		byEmail[row["email"].(string)] = row
	}
	require.Equal(t, "admin", byEmail[adminEmail]["role"])
	require.True(t, byEmail[adminEmail]["email_verified"].(bool))
	require.True(t, byEmail[adminEmail]["has_password"].(bool))
	require.Equal(t, "user", byEmail[memberEmail]["role"])

	// The search filter is the same statement's WHERE clause, so it finds one.
	status, payload, body = api.call(t, fiber.MethodGet, "/api/admin/users?q=member", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	require.Len(t, rows(t, payload), 1)

	memberID := byEmail[memberEmail]["id"].(string)

	// Promote: the row is written, and the answer is the row the list would show.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/admin/users/"+memberID+"/role",
		`{"role":"admin"}`, bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "admin", data(t, payload)["role"])
	require.Equal(t, "admin", roleOf(t, api.dsn, memberEmail))

	// Demote back. Two admins exist at this point, so the last-admin guard lets
	// it through.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/admin/users/"+memberID+"/role",
		`{"role":"user"}`, bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "user", data(t, payload)["role"])
	require.Equal(t, "user", roleOf(t, api.dsn, memberEmail))

	// A promotion the member did not ask for is now visible to them on /auth/me.
	status, payload, body = api.call(t, fiber.MethodPost, "/api/admin/users/"+memberID+"/role",
		`{"role":"admin"}`, bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)

	memberAccess, _ := api.login(t, memberEmail, "A-good-password1")
	status, payload, body = api.call(t, fiber.MethodGet, "/api/auth/me", "", bearer(memberAccess))
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "admin", data(t, payload)["role"], "the role travels on the profile, so the screen can offer the link")

	// And the newly promoted admin can reach the surface, which is what proves
	// the guard reads the table rather than the token.
	status, _, body = api.call(t, fiber.MethodGet, "/api/admin/overview", "", bearer(memberAccess))
	require.Equal(t, http.StatusOK, status, body)
}

// TestAnAdminCannotDemoteThemselves: a promotion is a decision about somebody
// else, so a mis-click cannot end the session that made it.
func TestAnAdminCannotDemoteThemselves(t *testing.T) {
	api := newAdminAPI(t)

	adminEmail := "admin@example.com"
	adminAccess := api.signUp(t, adminEmail, "A-good-password1")
	promoteByHand(t, api.dsn, adminEmail)

	status, payload, body := api.call(t, fiber.MethodGet, "/api/auth/me", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	adminID := data(t, payload)["id"].(string)

	status, payload, body = api.call(t, fiber.MethodPost, "/api/admin/users/"+adminID+"/role",
		`{"role":"user"}`, bearer(adminAccess))
	require.Equal(t, http.StatusConflict, status, body)
	require.Equal(t, "self_demotion", payload["error"].(map[string]any)["code"])
	require.Equal(t, "admin", roleOf(t, api.dsn, adminEmail), "the role is left alone")
}

// TestTheLastAdminStatementRefuses is the rule where it is enforced.
//
// It is asserted against the statement rather than through the API because the
// API cannot reach it: an admin demoting somebody else always has at least two
// admins in the table. What the statement is for is the moment between two
// demotions running at once, and a row count of zero is that refusal.
func TestTheLastAdminStatementRefuses(t *testing.T) {
	api := newAdminAPI(t)

	adminEmail := "admin@example.com"
	adminAccess := api.signUp(t, adminEmail, "A-good-password1")
	promoteByHand(t, api.dsn, adminEmail)

	status, payload, body := api.call(t, fiber.MethodGet, "/api/auth/me", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	adminID := data(t, payload)["id"].(string)

	pool, err := pgxpool.New(t.Context(), api.dsn)
	require.NoError(t, err)
	defer pool.Close()

	const demote = `UPDATE users
		SET role = 'user', updated_at = now()
		WHERE users.id = $1
		  AND users.role = 'admin'
		  AND (SELECT count(*) FROM users AS admins WHERE admins.role = 'admin') > 1`

	tag, err := pool.Exec(t.Context(), demote, adminID)
	require.NoError(t, err)
	require.Zero(t, tag.RowsAffected(), "the only admin cannot be demoted")
	require.Equal(t, "admin", roleOf(t, api.dsn, adminEmail))

	// With a second admin in the table the same statement goes through, which is
	// what makes the guard a guard rather than a refusal to demote at all.
	otherEmail := "other@example.com"
	api.signUp(t, otherEmail, "A-good-password1")
	promoteByHand(t, api.dsn, otherEmail)

	tag, err = pool.Exec(t.Context(), demote, adminID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected())
	require.Equal(t, "user", roleOf(t, api.dsn, adminEmail))
}

// TestTheOperatorSurfaceAsksForNoWorkspace is the boundary: an account that
// belongs to no workspace can still be administered, so the surface must answer
// without the tenant header the rest of the product requires.
func TestTheOperatorSurfaceAsksForNoWorkspace(t *testing.T) {
	api := newAdminAPI(t)

	adminEmail := "admin@example.com"
	adminAccess := api.signUp(t, adminEmail, "A-good-password1")
	promoteByHand(t, api.dsn, adminEmail)

	// No X-Workspace-Id header, and the account owns no workspace.
	status, payload, body := api.call(t, fiber.MethodGet, "/api/admin/users", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	accounts := rows(t, payload)
	require.Len(t, accounts, 1)
	require.Zero(t, accounts[0].(map[string]any)["workspaces"], "an account in no workspace counts zero")

	// A workspace id that is not a uuid is not a 400 here either: the surface does
	// not read it at all.
	status, _, body = api.call(t, fiber.MethodGet, "/api/admin/users", "", map[string]string{
		fiber.HeaderAuthorization: "Bearer " + adminAccess,
		"X-Workspace-Id":          "not-a-uuid",
	})
	require.Equal(t, http.StatusOK, status, body)
}

// TestTheAccountListCountsWorkspaces: the count is what tells an operator whether
// an account is in use, and it is read in one statement for the whole page.
func TestTheAccountListCountsWorkspaces(t *testing.T) {
	api := newAdminAPI(t)

	adminEmail := "admin@example.com"
	adminAccess := api.signUp(t, adminEmail, "A-good-password1")
	promoteByHand(t, api.dsn, adminEmail)

	// The admin onboards a workspace, which is one membership.
	status, payload, body := api.call(t, fiber.MethodPost, "/api/workspaces/onboard",
		`{"name":"Toko Sinar","business_field":"Retail","timezone":"Asia/Jakarta"}`, bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	require.NotEmpty(t, data(t, payload))

	status, payload, body = api.call(t, fiber.MethodGet, "/api/admin/users", "", bearer(adminAccess))
	require.Equal(t, http.StatusOK, status, body)
	accounts := rows(t, payload)
	require.Len(t, accounts, 1)
	require.Equal(t, float64(1), accounts[0].(map[string]any)["workspaces"])
}

// TestTheRoleColumnIsBounded: the check constraint is the last line of defence
// against a role nobody defined, and it is on the column rather than in the
// application.
func TestTheRoleColumnIsBounded(t *testing.T) {
	api := newAdminAPI(t)

	email := "admin@example.com"
	api.signUp(t, email, "A-good-password1")

	pool, err := pgxpool.New(t.Context(), api.dsn)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(t.Context(), "UPDATE users SET role = 'superuser' WHERE lower(email) = lower($1)", email)
	require.Error(t, err, "a role the surface does not know must not be storable")

	_, err = pool.Exec(t.Context(), "UPDATE users SET role = 'admin' WHERE lower(email) = lower($1)", email)
	require.NoError(t, err)
}
