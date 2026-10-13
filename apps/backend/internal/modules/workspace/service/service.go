// Package service implements the workspace use cases: onboarding, member
// management, and invitations. It depends on the domain contracts and the
// platform primitives only.
package service

import (
	"context"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/robzlabz/employeebot/apps/backend/internal/modules/workspace/domain"
)

// Config holds the workspace settings.
type Config struct {
	// InvitationTTL is how long an invitation link stays valid.
	InvitationTTL time.Duration
	// FrontendURL is the base URL used to build the invitation link.
	FrontendURL string
	// DefaultTimezone is used when the onboarding form omits one.
	DefaultTimezone string
	// Clock is injectable so tests can control expiry.
	Clock func() time.Time
	// Logger reports a trail that could not be written. Optional.
	Logger *zap.Logger
}

// Deps are the service dependencies. The provisioner is the seam EPIC 3 fills
// with the Bolu template copy.
type Deps struct {
	Repository domain.Repository
	// Mailer delivers invitation links. Optional.
	Mailer domain.Mailer
	// Tokens generates the random invitation token and its hash.
	Tokens domain.Tokens
	// Limits reports how many members the workspace's package allows. Optional:
	// unwired means the deployment sells no packages and nothing is capped.
	Limits domain.PlanLimits
	// Auditor records membership and role changes in the workspace's trail. Optional:
	// unwired means the change happens and leaves no line, which is what a deployment
	// without the approval module runs with.
	Auditor domain.Auditor
	Config  Config
}

type service struct {
	deps    domain.Repository
	mail    domain.Mailer
	tokens  domain.Tokens
	limits  domain.PlanLimits
	auditor domain.Auditor
	cfg     Config
}

// New builds the workspace service.
func New(deps Deps) domain.Service {
	if deps.Config.Clock == nil {
		deps.Config.Clock = time.Now
	}
	if deps.Config.InvitationTTL <= 0 {
		deps.Config.InvitationTTL = 7 * 24 * time.Hour
	}
	if deps.Config.DefaultTimezone == "" {
		deps.Config.DefaultTimezone = "Asia/Jakarta"
	}
	if deps.Config.Logger == nil {
		deps.Config.Logger = zap.NewNop()
	}
	return &service{
		deps:    deps.Repository,
		mail:    deps.Mailer,
		tokens:  deps.Tokens,
		limits:  deps.Limits,
		auditor: deps.Auditor,
		cfg:     deps.Config,
	}
}

// Onboard creates the workspace, makes the caller its owner, and creates the two
// default teams. Submitting twice returns the same workspace.
func (s *service) Onboard(ctx context.Context, userID uuid.UUID, req domain.OnboardRequest) (domain.OnboardResult, error) {
	if userID == uuid.Nil {
		return domain.OnboardResult{}, domain.ErrNotMember
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.OnboardResult{}, fmt.Errorf("%w: business name is required", domain.ErrInvalidInput)
	}

	timezone := strings.TrimSpace(req.Timezone)
	if timezone == "" {
		timezone = s.cfg.DefaultTimezone
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return domain.OnboardResult{}, fmt.Errorf("%w: unknown timezone %q", domain.ErrInvalidInput, timezone)
	}

	result, err := s.deps.Onboard(ctx, userID, domain.OnboardRequest{
		Name:          name,
		BusinessField: strings.TrimSpace(req.BusinessField),
		Timezone:      timezone,
	})
	if err != nil {
		return domain.OnboardResult{}, err
	}

	// The Bolu templates are copied by the repository, inside the same
	// transaction that creates the workspace, so the result already carries the
	// agents a caller would expect to find.
	return result, nil
}

// Workspaces lists the workspaces the caller belongs to.
func (s *service) Workspaces(ctx context.Context, userID uuid.UUID) ([]domain.Workspace, error) {
	return s.deps.Workspaces(ctx, userID)
}

// Current reads the active workspace.
func (s *service) Current(ctx context.Context, scope domain.Scope) (domain.Workspace, error) {
	return s.deps.Workspace(ctx, scope)
}

// Update changes the workspace profile. Only an owner may do it.
//
// Every field is validated here rather than in the handler, because the same
// values decide when the approval gate lets an action leave: a window that is
// malformed, or a week with no working day in it, would park every draft forever.
func (s *service) Update(ctx context.Context, scope domain.Scope, actorRole string, req domain.UpdateRequest) (domain.Workspace, error) {
	if !domain.CanAdminister(actorRole) {
		return domain.Workspace{}, domain.ErrForbidden
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.Workspace{}, fmt.Errorf("%w: business name is required", domain.ErrInvalidInput)
	}

	timezone := strings.TrimSpace(req.Timezone)
	if timezone == "" {
		timezone = s.cfg.DefaultTimezone
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return domain.Workspace{}, fmt.Errorf("%w: unknown timezone %q", domain.ErrInvalidInput, timezone)
	}

	language := strings.TrimSpace(req.Language)
	if language == "" {
		language = languageIndonesian
	}
	if !validLanguages[language] {
		return domain.Workspace{}, fmt.Errorf("%w: unknown reply language %q", domain.ErrInvalidInput, language)
	}

	start, err := normaliseClock(req.WorkHoursStart)
	if err != nil {
		return domain.Workspace{}, err
	}
	end, err := normaliseClock(req.WorkHoursEnd)
	if err != nil {
		return domain.Workspace{}, err
	}
	if start == end {
		return domain.Workspace{}, fmt.Errorf("%w: working hours start and end are the same", domain.ErrInvalidInput)
	}

	days, err := normaliseDays(req.WorkDays)
	if err != nil {
		return domain.Workspace{}, err
	}

	return s.deps.UpdateWorkspace(ctx, scope, domain.UpdateRequest{
		Name:           name,
		BusinessField:  strings.TrimSpace(req.BusinessField),
		Timezone:       timezone,
		Language:       language,
		WorkHoursStart: start,
		WorkHoursEnd:   end,
		WorkDays:       days,
	})
}

// The reply languages a workspace may choose.
const languageIndonesian = "id"

var validLanguages = map[string]bool{languageIndonesian: true, "en": true, "both": true}

// normaliseClock validates a "HH:MM" time of day and returns it zero-padded.
//
// The database column is `time`, so the string is checked here instead of being
// handed to the driver: a value it would refuse surfaces as a 500 rather than as
// the 400 the form deserves.
func normaliseClock(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	var hour, minute int
	if _, err := fmt.Sscanf(trimmed, "%d:%d", &hour, &minute); err != nil {
		return "", fmt.Errorf("%w: working hours must be HH:MM", domain.ErrInvalidInput)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return "", fmt.Errorf("%w: %q is not a time of day", domain.ErrInvalidInput, trimmed)
	}

	return fmt.Sprintf("%02d:%02d", hour, minute), nil
}

// normaliseDays validates the working week and returns it sorted and deduplicated.
//
// An empty week is refused rather than treated as "every day": a workspace that
// closed every day would hold every outbound action forever, and saying so at the
// form is the only place that is fixable.
func normaliseDays(days []int) ([]int, error) {
	if len(days) == 0 {
		return nil, fmt.Errorf("%w: pick at least one working day", domain.ErrInvalidInput)
	}

	seen := make(map[int]bool, len(days))
	out := make([]int, 0, len(days))
	for _, day := range days {
		if day < 1 || day > 7 {
			return nil, fmt.Errorf("%w: %d is not a weekday", domain.ErrInvalidInput, day)
		}
		if seen[day] {
			continue
		}
		seen[day] = true
		out = append(out, day)
	}
	sort.Ints(out)

	return out, nil
}

// Teams lists the teams of the active workspace.
func (s *service) Teams(ctx context.Context, scope domain.Scope) ([]domain.Team, error) {
	return s.deps.Teams(ctx, scope)
}

// Members lists the members of the active workspace.
func (s *service) Members(ctx context.Context, scope domain.Scope) ([]domain.Member, error) {
	return s.deps.Members(ctx, scope)
}

// SetRole changes a member's role. Owners and admins may manage members, but
// only an owner may grant the owner role or change another owner.
func (s *service) SetRole(ctx context.Context, scope domain.Scope, actorRole string, userID uuid.UUID, role string) (domain.Member, error) {
	if !domain.CanManageMembers(actorRole) {
		return domain.Member{}, domain.ErrForbidden
	}
	if !domain.IsValidRole(role) {
		return domain.Member{}, domain.ErrInvalidRole
	}
	if role == domain.RoleOwner && !domain.CanAdminister(actorRole) {
		return domain.Member{}, domain.ErrForbidden
	}
	if err := s.guardOwnerTarget(ctx, scope, actorRole, userID); err != nil {
		return domain.Member{}, err
	}

	member, err := s.deps.SetRole(ctx, scope, userID, role)
	if err != nil {
		return domain.Member{}, err
	}

	// A role decides who may do what inside the workspace, so the change is recorded with
	// both what it was and what it became: "who was an owner on Tuesday" is the question,
	// and the previous value is what answers it.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "member.role_changed",
		ObjectType:  "member",
		ObjectID:    userID.String(),
		ActorUserID: scope.UserID,
		Metadata:    map[string]any{"role": role},
	})

	return member, nil
}

// RemoveMember deletes a membership. A member cannot remove anyone, an admin
// cannot remove an owner, and nobody can remove the last owner.
func (s *service) RemoveMember(ctx context.Context, scope domain.Scope, actorRole string, actorID, userID uuid.UUID) error {
	if !domain.CanManageMembers(actorRole) {
		return domain.ErrForbidden
	}
	if actorID == userID {
		return domain.ErrCannotRemoveYourself
	}
	if err := s.guardOwnerTarget(ctx, scope, actorRole, userID); err != nil {
		return err
	}

	// The role is read before the row goes, because after it the trail can only say that
	// somebody left — not what access they had held. It is read only when something will
	// record it: a deployment with no trail should not pay a query for a line nobody writes.
	held := ""
	if s.auditor != nil {
		held = s.roleOf(ctx, scope, userID)
	}

	if err := s.deps.RemoveMember(ctx, scope, userID); err != nil {
		return err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "member.removed",
		ObjectType:  "member",
		ObjectID:    userID.String(),
		ActorUserID: scope.UserID,
		Metadata:    map[string]any{"had_role": held},
	})

	return nil
}

// roleOf reads one member's role for a trail entry, answering the empty string when the
// member cannot be read. A trail line with no previous role is worse than none, so the
// caller records what it has rather than refusing the removal.
func (s *service) roleOf(ctx context.Context, scope domain.Scope, userID uuid.UUID) string {
	members, err := s.deps.Members(ctx, scope)
	if err != nil {
		return ""
	}
	for _, member := range members {
		if member.UserID == userID {
			return member.Role
		}
	}
	return ""
}

// audit records one entry. A trail that could not be written is reported rather than
// failing the change: the membership already changed, and refusing it afterwards would
// leave the workspace in a state nobody asked for.
func (s *service) audit(ctx context.Context, scope domain.Scope, entry domain.AuditEntry) {
	if s.auditor == nil {
		return
	}
	if err := s.auditor.Record(ctx, scope, entry); err != nil {
		s.log().Warn("could not record a workspace audit entry",
			zap.String("action", entry.Action), zap.Error(err))
	}
}

// log is the service's logger, which is never nil.
func (s *service) log() *zap.Logger { return s.cfg.Logger }

// Invite sends an invitation. Only owners and admins may invite, and the role
// they may grant is capped by their own: an admin cannot create an owner.
// checkMemberLimit refuses an invitation the workspace's package does not allow.
//
// It counts the people who are there plus the invitations that are still open,
// because an invitation is a seat somebody will take: counting only the members
// would let a workspace invite ten people into a package that allows three, and the
// last seven would fail at acceptance — after they were told to expect access.
func (s *service) checkMemberLimit(ctx context.Context, scope domain.Scope) error {
	if s.limits == nil {
		return nil
	}

	limit, err := s.limits.MaxMembers(ctx, scope.WorkspaceID)
	if err != nil {
		return err
	}
	if limit <= 0 {
		return nil
	}

	members, err := s.deps.Members(ctx, scope)
	if err != nil {
		return err
	}
	invitations, err := s.deps.Invitations(ctx, scope)
	if err != nil {
		return err
	}

	if len(members)+len(invitations) >= limit {
		return fmt.Errorf("%w: paket ini mengizinkan %d anggota; naikkan paket untuk menambah",
			domain.ErrPlanLimit, limit)
	}

	return nil
}

func (s *service) Invite(ctx context.Context, scope domain.Scope, actorRole string, actorID uuid.UUID, email, role string) (domain.Invitation, error) {
	if !domain.CanManageMembers(actorRole) {
		return domain.Invitation{}, domain.ErrForbidden
	}
	if !domain.IsValidRole(role) {
		return domain.Invitation{}, domain.ErrInvalidRole
	}
	if role == domain.RoleOwner && !domain.CanAdminister(actorRole) {
		return domain.Invitation{}, domain.ErrForbidden
	}

	normalized, err := normalizeEmail(email)
	if err != nil {
		return domain.Invitation{}, err
	}
	if err := s.checkMemberLimit(ctx, scope); err != nil {
		return domain.Invitation{}, err
	}

	raw, hash, err := s.tokens.Generate()
	if err != nil {
		return domain.Invitation{}, err
	}

	invitation, err := s.deps.CreateInvitation(ctx, scope, domain.NewInvitation{
		Email:     normalized,
		Role:      role,
		TokenHash: hash,
		InvitedBy: actorID,
		ExpiresAt: s.cfg.Clock().Add(s.cfg.InvitationTTL),
	})
	if err != nil {
		return domain.Invitation{}, err
	}

	if err := s.sendInvitation(ctx, normalized, raw, invitation); err != nil {
		return domain.Invitation{}, err
	}

	// An invitation is access that has not been taken yet, so it belongs in the trail
	// beside a role change. The address is deliberately *not* recorded: the line names the
	// invitation by id and the role it grants, and an address is personal data this
	// nothing needs.
	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "member.invited",
		ObjectType:  "invitation",
		ObjectID:    invitation.ID.String(),
		ActorUserID: actorID,
		Metadata:    map[string]any{"role": role},
	})

	return invitation, nil
}

// Invitations lists the pending invitations of the active workspace.
func (s *service) Invitations(ctx context.Context, scope domain.Scope) ([]domain.Invitation, error) {
	return s.deps.Invitations(ctx, scope)
}

// RevokeInvitation deletes a pending invitation.
func (s *service) RevokeInvitation(ctx context.Context, scope domain.Scope, actorRole string, id uuid.UUID) error {
	if !domain.CanManageMembers(actorRole) {
		return domain.ErrForbidden
	}

	if err := s.deps.RevokeInvitation(ctx, scope, id); err != nil {
		return err
	}

	s.audit(ctx, scope, domain.AuditEntry{
		Action:      "member.invitation_revoked",
		ObjectType:  "invitation",
		ObjectID:    id.String(),
		ActorUserID: scope.UserID,
	})

	return nil
}

// AcceptInvitation consumes an invitation for the signed-in account.
func (s *service) AcceptInvitation(ctx context.Context, token string, userID uuid.UUID, email string) (domain.Workspace, error) {
	if strings.TrimSpace(token) == "" {
		return domain.Workspace{}, domain.ErrInvitationInvalid
	}
	if userID == uuid.Nil {
		return domain.Workspace{}, domain.ErrInvitationInvalid
	}

	return s.deps.AcceptInvitation(ctx, domain.HashToken(token), userID, strings.ToLower(strings.TrimSpace(email)))
}

// Resolve returns the caller's role, which is what the tenant middleware uses.
func (s *service) Resolve(ctx context.Context, workspaceID, userID uuid.UUID) (string, error) {
	if workspaceID == uuid.Nil || userID == uuid.Nil {
		return "", domain.ErrNotMember
	}
	return s.deps.Membership(ctx, domain.Scope{WorkspaceID: workspaceID, UserID: userID}, userID)
}

// guardOwnerTarget stops an admin from touching an owner: only an owner may
// re-role or remove another owner.
func (s *service) guardOwnerTarget(ctx context.Context, scope domain.Scope, actorRole string, userID uuid.UUID) error {
	if domain.CanAdminister(actorRole) {
		return nil
	}

	members, err := s.deps.Members(ctx, scope)
	if err != nil {
		return err
	}
	for _, member := range members {
		if member.UserID == userID && member.Role == domain.RoleOwner {
			return domain.ErrForbidden
		}
	}
	return nil
}

func (s *service) sendInvitation(ctx context.Context, email, token string, invitation domain.Invitation) error {
	if s.mail == nil {
		return nil
	}

	link := fmt.Sprintf("%s/join?token=%s", strings.TrimRight(s.cfg.FrontendURL, "/"), token)
	body := fmt.Sprintf("Kamu diundang bergabung ke workspace Bolu sebagai %s.\n\nBuka tautan ini untuk menerima:\n\n%s\n\nTautan berlaku sampai %s.",
		invitation.Role, link, invitation.ExpiresAt.UTC().Format("2 January 2006 15:04 MST"))

	return s.mail.Send(ctx, email, "Undangan bergabung ke Bolu", body)
}

func normalizeEmail(email string) (string, error) {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" {
		return "", fmt.Errorf("%w: email is required", domain.ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(trimmed); err != nil {
		return "", fmt.Errorf("%w: email is not valid", domain.ErrInvalidInput)
	}
	return strings.ToLower(trimmed), nil
}

var _ domain.Service = (*service)(nil)
