// Package domain defines the operator surface's contract: the accounts a
// deployment has, and the one decision an admin makes about them.
//
// The module exists because "who may reach /admin" is not a question any other
// module can answer. A workspace role is granted by whoever owns that workspace,
// and an account with no workspace at all still has to be administrable; so this
// is the one surface whose subject is the deployment rather than a tenant.
//
// Three rules are the whole reason it is more than a table read:
//
//   - **Accounts only.** Every read here is `users`. Bolus, tasks, conversations
//     and money belong to a workspace and stay behind the tenant policies, so a
//     platform admin administers the deployment without becoming a reader of
//     every customer's data. That is a boundary, not an omission.
//   - **The last admin stays.** Demoting the only account that may reach the
//     surface would leave the deployment with no way back in short of a
//     statement on the database, so it is refused.
//   - **Nobody demotes themselves.** A promotion is a decision about somebody
//     else; an admin who wants out asks another admin. It is what keeps a
//     mis-click from ending the session that made it.
package domain

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	authdomain "github.com/robzlabz/employeebot/apps/backend/internal/modules/auth/domain"
)

// Errors this module returns. Handlers map them onto status codes, so the
// mapping lives in one place instead of being guessed per endpoint.
var (
	// ErrNotAdmin means the caller may not reach the operator surface at all.
	ErrNotAdmin = errors.New("admin: the caller is not a platform admin")
	// ErrAccountNotFound means no account has that id.
	ErrAccountNotFound = errors.New("admin: account not found")
	// ErrLastAdmin means the change would leave the deployment with no admin.
	ErrLastAdmin = errors.New("admin: the last platform admin cannot be demoted")
	// ErrSelfDemotion means the caller tried to demote their own account.
	ErrSelfDemotion = errors.New("admin: an admin cannot demote their own account")
	// ErrInvalidInput means the request itself is wrong: a role that is not one
	// of the two, or a page outside its bounds.
	ErrInvalidInput = errors.New("admin: invalid input")
	// ErrNotConfigured means the deployment has no database, so the surface
	// answers 503 rather than failing the query.
	ErrNotConfigured = errors.New("admin: database is not configured")
)

// The page bounds a list read is clamped to, so one request cannot ask for the
// whole table.
const (
	DefaultPageSize = 25
	MaxPageSize     = 100
)

// Account is one row of the account list: what an operator reads about an
// account, and nothing that would let them use it.
type Account struct {
	ID            uuid.UUID
	Email         string
	Role          string
	EmailVerified bool
	Onboarded     bool
	HasGoogle     bool
	HasPassword   bool
	CreatedAt     time.Time
	// Workspaces is how many workspaces the account is a member of. It is a
	// count rather than a list: an operator needs to know whether an account is
	// in use, and which workspaces it is in is the customer's business.
	Workspaces int64
}

// Admin reports whether this account may reach the operator surface.
func (a Account) Admin() bool { return authdomain.IsAdmin(a.Role) }

// IsZero reports whether this is no account at all, which is what a read answers
// for an id that does not exist.
func (a Account) IsZero() bool { return a.ID == uuid.Nil }

// Stats is what the accounts add up to: the overview's numbers, counted over the
// whole table in one pass.
type Stats struct {
	Total          int64
	Admins         int64
	Users          int64
	Verified       int64
	Onboarded      int64
	GoogleAccounts int64
	NewLast7Days   int64
}

// Filter narrows a list read: an address search, a role, and the page.
type Filter struct {
	Search string
	Role   string
	Limit  int
	Offset int
}

// Normalised returns the filter with its bounds applied, so every caller reads
// the same page size and the same offset for the same request.
func (f Filter) Normalised() Filter {
	if f.Limit <= 0 {
		f.Limit = DefaultPageSize
	}
	if f.Limit > MaxPageSize {
		f.Limit = MaxPageSize
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return f
}

// Valid reports whether the filter asks for something that exists. Only the role
// can be wrong: an unknown role is a mistake rather than an empty page.
func (f Filter) Valid() bool {
	return f.Role == "" || f.Role == authdomain.RoleUser || f.Role == authdomain.RoleAdmin
}

// Repository is the persistence contract for the account list and the one
// decision the surface makes about an account.
type Repository interface {
	Users(ctx context.Context, filter Filter) ([]Account, error)
	CountUsers(ctx context.Context, filter Filter) (int64, error)
	Stats(ctx context.Context) (Stats, error)
	// AccountByID reads one account. A missing id is ErrAccountNotFound.
	AccountByID(ctx context.Context, id uuid.UUID) (Account, error)
	// Promote makes an account a platform admin and answers with it.
	Promote(ctx context.Context, id uuid.UUID) (Account, error)
	// Demote takes the platform role away. It answers ErrLastAdmin when the
	// account is the only one that may reach the surface, because the rule is
	// enforced by the statement rather than read and then written.
	Demote(ctx context.Context, id uuid.UUID) (Account, error)
	// WorkspaceCounts answers, per account id, how many workspaces it belongs to.
	// Ids with no membership are absent rather than zero.
	WorkspaceCounts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int64, error)
}

// ChangeRoleRequest is a promotion or a demotion.
type ChangeRoleRequest struct {
	// ActorID is the admin making the change, taken from the session rather than
	// from the request body.
	ActorID  uuid.UUID
	TargetID uuid.UUID
	Role     string
}

// Service is the operator surface's use case contract.
type Service interface {
	// Overview answers what the deployment's accounts add up to.
	Overview(ctx context.Context) (Stats, error)
	// List answers a page of accounts, with the total the filter matches.
	List(ctx context.Context, filter Filter) ([]Account, int64, error)
	// ChangeRole promotes or demotes an account.
	ChangeRole(ctx context.Context, req ChangeRoleRequest) (Account, error)
}
