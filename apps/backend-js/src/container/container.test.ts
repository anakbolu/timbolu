/**
 * A new workspace begins on the trial package, created inside the onboarding
 * transaction by the billing module. The workspace repository asks for it through
 * `SubscriptionProvisioner`, and nothing calls `startTrial` unless something
 * installs that provisioner: the workspace then exists with no subscription, and
 * `GET /api/billing/subscription` answers `billing_not_found` for it forever.
 *
 * The two cases pin both halves of that wiring. The first drives the repository
 * with a fake transaction and a recording provisioner, so it says the call lands
 * inside the onboarding transaction and after the workspace row. The second builds
 * the real container over a fake pool, so it says the container is what installs
 * the provisioner — the half that was missing.
 */

import { describe, expect, test } from "bun:test";

import type { Database, Scope, Tx } from "../platform/database/index.ts";
import { loadFromFile } from "../platform/config/init.ts";
import { PostgresRepository as WorkspaceRepository } from "../modules/workspace/repository/postgres.ts";
import { newContainer } from "./container.ts";

const userId = "22222222-2222-2222-2222-222222222222";
const workspaceId = "11111111-1111-1111-1111-111111111111";

const request = { name: "Toko Bolu", businessField: "retail", timezone: "Asia/Jakarta" };

/** One statement the fake driver was asked to run. */
interface Statement {
  text: string;
  values: unknown[];
}

/** Answers one statement from its text. */
type Answers = (text: string) => unknown[];

/**
 * The transaction the repositories run in.
 *
 * postgres.js tags a template literal onto the object it hands the callback, so
 * the fake is a callable that records every statement. The cast names the shape
 * the driver already guarantees: the repositories only ever use it as a tag.
 */
class FakeTx {
  readonly statements: Statement[] = [];

  constructor(private readonly answers: Answers) {}

  readonly tag = ((strings: TemplateStringsArray, ...values: unknown[]) => {
    const text = strings.join("$").replace(/\s+/g, " ").trim();
    this.statements.push({ text, values });
    return Promise.resolve(this.answers(text));
  }) as unknown as Tx;
}

/**
 * The pool, reduced to the one thing onboarding uses it for. `Database` carries
 * a driver handle the repositories here never touch, so the cast is the whole
 * difference between this and the real thing.
 */
class FakeDatabase {
  readonly tx: FakeTx;
  /** The same object, presented as the pool the repositories are built over. */
  readonly database: Database;

  constructor(answers: Answers) {
    this.tx = new FakeTx(answers);
    this.database = this as unknown as Database;
  }

  async inScope<T>(_scope: Scope, fn: (tx: Tx) => Promise<T>): Promise<T> {
    return fn(this.tx.tag);
  }
}

const workspaceRow = {
  id: workspaceId,
  name: request.name,
  business_field: request.businessField,
  timezone: request.timezone,
  language: "id",
  work_hours_start: "08:00",
  work_hours_end: "17:00",
  created_at: new Date(0),
  updated_at: new Date(0),
  owner_user_id: userId,
  work_days: [1, 2, 3, 4, 5],
};

const teamRow = {
  id: "33333333-3333-3333-3333-333333333333",
  workspace_id: workspaceId,
  name: "Tim Bolu",
  kind: "bolu",
  created_at: new Date(0),
  updated_at: new Date(0),
};

const trialPlanRow = {
  id: "44444444-4444-4444-4444-444444444444",
  key: "trial",
  name: "Trial",
  monthly_price_idr: 0,
  token_quota: 100000,
  daily_cost_micros: 0,
  period_days: 14,
  max_agents: 1,
  max_integrations: 1,
  max_members: 1,
  is_trial: true,
  sort_order: 1,
  created_at: new Date(0),
};

const trialSubscriptionRow = {
  id: "55555555-5555-5555-5555-555555555555",
  workspace_id: workspaceId,
  plan_id: trialPlanRow.id,
  status: "trial",
  current_period_start: new Date(0),
  current_period_end: new Date(14 * 24 * 60 * 60 * 1000),
  grace_until: new Date(21 * 24 * 60 * 60 * 1000),
  past_due_since: null,
  pending_plan_id: null,
  past_due_notified_at: null,
  read_only_notified_at: null,
  canceled_at: null,
  created_at: new Date(0),
  updated_at: new Date(0),
};

/** The statements onboarding and the trial issue, answered in order. */
function answers(text: string): unknown[] {
  if (text.includes("FROM workspaces") && text.includes("owner_user_id")) return [];
  if (text.startsWith("INSERT INTO workspaces")) return [workspaceRow];
  if (text.startsWith("INSERT INTO teams")) return [teamRow];
  if (text.startsWith("FROM plans") || text.includes("FROM plans")) return [trialPlanRow];
  if (text.startsWith("INSERT INTO subscriptions")) return [trialSubscriptionRow];
  return [];
}

describe("workspace onboarding", () => {
  test("starts the trial inside the onboarding transaction, after the workspace row", async () => {
    const db = new FakeDatabase(answers);
    const repository = new WorkspaceRepository(db.database);

    const started: Array<{ workspaceId: string; tx: Tx; statementsBefore: number }> = [];
    repository.setSubscriptionProvisioner({
      async startTrial(tx: Tx, id: string): Promise<void> {
        started.push({ workspaceId: id, tx, statementsBefore: db.tx.statements.length });
      },
    });

    const result = await repository.onboard(userId, request);

    expect(result.created).toBe(true);
    expect(result.workspace.id).toBe(workspaceId);
    expect(started).toHaveLength(1);
    expect(started[0].workspaceId).toBe(workspaceId);

    // The provisioner is handed the onboarding transaction itself, so the package
    // and the workspace commit or roll back together.
    expect(started[0].tx).toBe(db.tx.tag);

    const insertedAt = db.tx.statements.findIndex((statement) =>
      statement.text.startsWith("INSERT INTO workspaces"),
    );
    expect(insertedAt).toBeGreaterThanOrEqual(0);
    expect(started[0].statementsBefore).toBeGreaterThan(insertedAt);
  });
});

describe("container", () => {
  test("installs the billing repository as the trial provisioner", async () => {
    // The local defaults, minus everything that would open a socket: the pool is
    // the fake below, and the log is silenced so the case reads as one assertion.
    const config = loadFromFile("");
    config.logging.level = "silent";
    config.database.url = "";
    config.redis.url = "";
    config.temporal.hostPort = "";
    config.storage.localRoot = "";
    config.kms.masterKey = "";
    config.llm.secretEncryptionKey = "";

    const db = new FakeDatabase(answers);
    const container = await newContainer({ config, db: db.database });

    const repository = container.workspaceRepository;
    if (!repository) throw new Error("the container built no workspace repository");

    const result = await repository.onboard(userId, request);
    expect(result.created).toBe(true);

    // The trial only reaches the database when the container wired the billing
    // repository in; without it onboarding writes a workspace and nothing else.
    const subscription = db.tx.statements.find((statement) =>
      statement.text.startsWith("INSERT INTO subscriptions"),
    );
    expect(subscription).toBeDefined();
    expect(subscription?.values[0]).toBe(workspaceId);
  });
});
