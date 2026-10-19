/**
 * The operator surface's business rules: who may read the account list, and
 * which changes to a role are refused.
 *
 * Ported from internal/modules/admin/service/service.go.
 *
 * Three refusals are the module's whole reason for having a service:
 *   - the caller must be an admin (the middleware checks it too, and this is the
 *     second lock, read from the database rather than from the request);
 *   - the last admin stays;
 *   - nobody demotes themselves.
 */

import type { Account, ChangeRoleRequest, Filter, Repository, Service as ServicePort, Stats } from "../domain/admin.ts";
import {
  AdminError,
  ErrInvalidInput,
  ErrNotAdmin,
  ErrNotConfigured,
  ErrSelfDemotion,
  RoleAdmin,
  RoleUser,
  accountIsAdmin,
  filterIsValid,
  normaliseFilter,
} from "../domain/admin.ts";

/** What the service is built from. */
export interface Deps {
  repository: Repository;
}

/** Implements the domain service contract. */
export class Service implements ServicePort {
  private readonly repository: Repository | null;

  constructor(deps: Deps) {
    this.repository = deps.repository ?? null;
  }

  /** Answers what the deployment's accounts add up to. */
  async overview(): Promise<Stats> {
    return this.ready().stats();
  }

  /**
   * Answers a page of accounts, with the total the filter matches.
   *
   * The workspace counts are fetched for the page's ids in one statement rather
   * than per row, because a list of twenty-five accounts should not be
   * twenty-six queries.
   */
  async list(filter: Filter): Promise<[Account[], number]> {
    const repo = this.ready();
    if (!filterIsValid(filter)) throw invalidRole();

    const normalised = normaliseFilter(filter);
    const accounts = await repo.users(normalised);
    const total = await repo.countUsers(normalised);

    if (accounts.length > 0) {
      const counts = await repo.workspaceCounts(accounts.map((account) => account.id));
      for (const account of accounts) account.workspaces = counts[account.id] ?? 0;
    }

    return [accounts, total];
  }

  /**
   * Promotes or demotes an account.
   *
   * The order of the checks is the order the reasons are worth explaining in:
   * the caller, then the request, then the account, then the two rules about a
   * demotion. The caller is read from the database rather than from a value the
   * request carries, so the service is not a way around the middleware.
   */
  async changeRole(req: ChangeRoleRequest): Promise<Account> {
    const repo = this.ready();
    if (req.role !== RoleUser && req.role !== RoleAdmin) throw invalidRole();

    const actor = await repo.accountByID(req.actorID);
    if (!accountIsAdmin(actor)) throw ErrNotAdmin;

    const target = await repo.accountByID(req.targetID);

    // The role is already what was asked for: a repeated promotion is the same
    // request twice, and answering it with the account is what makes a retry safe.
    if (target.role === req.role) return this.withWorkspaceCount(repo, target);

    // An admin who wants out asks another admin. Checked here rather than in the
    // statement because it is about the caller, which the statement does not see.
    if (req.role === RoleUser && req.targetID === req.actorID) throw ErrSelfDemotion;

    // The last admin is refused by the demotion statement itself, so the rule
    // holds under two demotions running at once.
    const updated = req.role === RoleAdmin ? await repo.promote(req.targetID) : await repo.demote(req.targetID);
    return this.withWorkspaceCount(repo, updated);
  }

  /**
   * Fills in how many workspaces an account belongs to, so a promotion answers
   * with the row the list would show. A count that cannot be read is left at
   * zero rather than failing the change: the role is already written.
   */
  private async withWorkspaceCount(repo: Repository, account: Account): Promise<Account> {
    try {
      const counts = await repo.workspaceCounts([account.id]);
      account.workspaces = counts[account.id] ?? 0;
    } catch {
      // The role is written; a number the screen can reload is not worth a 500.
    }
    return account;
  }

  /**
   * Whether the service has a repository to work with. A deployment fact rather
   * than a request one: an API without a database answers 503 for this surface.
   */
  private ready(): Repository {
    if (!this.repository) throw ErrNotConfigured;
    return this.repository;
  }
}

/** `fmt.Errorf("%w: role must be %q or %q", ErrInvalidInput, ...)`. */
function invalidRole(): AdminError {
  return new AdminError(ErrInvalidInput.kind, `${ErrInvalidInput.message}: role must be "${RoleUser}" or "${RoleAdmin}"`);
}
