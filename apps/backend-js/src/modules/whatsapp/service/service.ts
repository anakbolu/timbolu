/**
 * The WhatsApp use cases: linking a number, who answers it, what a customer's
 * message becomes, and how a reply is paced on its way out.
 *
 * Four rules run through the package:
 *
 *   - a number is linked once, and the session behind it is the number's rather
 *     than the tenant's.
 *   - nothing leaves without an approved draft. This package carries a decision
 *     out; it never makes one. The one exception is the pace, and a pace is not
 *     a decision.
 *   - a message is recorded before it is acted on.
 *   - a hold is not a failure.
 *
 * Ported 1:1 from internal/modules/whatsapp/service/service.go; the message half
 * lives in messages.ts, as it does in the Go package.
 */

import type { Logger } from "../../../platform/logger.ts";
import type {
  Activity,
  Alerter,
  Inbound,
  Link,
  Media,
  Number,
  Outbox,
  OutboxFilter,
  MessageFilter,
  Replies,
  Scope,
  SessionForgetter,
  Store,
  Transport,
} from "../domain/whatsapp.ts";
import {
  AlertDisconnected,
  AlertNeedsRelink,
  ErrAlreadyLinked,
  ErrInvalidInput,
  ErrLinkExpired,
  ErrNotFound,
  ErrNotConfigured,
  ErrNoAgent,
  ErrNotConnected,
  ErrTermsNotAccepted,
  LinkExpired,
  LinkFailed,
  LinkPaired,
  NilUUID,
  StatusConnected,
  StatusDisconnected,
  StatusLinking,
  StatusNeedsRelink,
  StatusUnlinked,
  is,
  numberLinked,
  scopeIsZero,
  wrap,
} from "../domain/whatsapp.ts";
import type { Claimed, Connections, ReportRequest, Watchers } from "../domain/engine.ts";
import type { EnqueueRequest, LinkRequest, ServicePort } from "../domain/service.ts";
import {
  claim as claimMessages,
  enqueue as enqueueMessage,
  failed as failedMessage,
  history as historyMessages,
  messages as listMessages,
  numberByIntegration as numberByIntegrationID,
  outbox as listOutbox,
  pendingMedia as pendingMediaMessages,
  receive as receiveMessage,
  reclaim as reclaimMessages,
  retry as retryMessage,
  sent as sentMessage,
  storeMedia as storeMediaMessage,
  type MessageDeps,
} from "./messages.ts";

/** The application key a linked number appears under in the hub. */
export const App = "whatsapp";

/** The ceiling a number gets when the workspace did not choose one. */
export const DefaultHourlyLimit = 40;

/**
 * How long a message waits when the hourly ceiling stopped it. Deliberately not
 * the full hour: the ceiling is counted over a sliding hour.
 */
const holdDuration = 5 * 60 * 1000;

/** How long a finished link attempt is remembered. */
const linkStateTTL = 2 * 60 * 1000;

/** The service dependencies. */
export interface Deps {
  repository: Store;
  /** Forgets a device when a number is unlinked. */
  sessions: SessionForgetter;
  /** The WhatsApp side. Optional: a deployment with no gateway serves reads. */
  transport?: Transport | null;
  /** Records a number as a connection in the hub. */
  connections?: Connections | null;
  /** Publishes what happened into the workspace's live stream. */
  activity?: Activity | null;
  /** Tells the owners when a number needs them. */
  alerter?: Alerter | null;
  /** Opens the task an inbound message asks for. */
  replies?: Replies | null;
  /** Starts the routines watching the message event. */
  watchers?: Watchers | null;
  /** Reads the bytes of what a customer sent. */
  media?: { get(key: string): Promise<Uint8Array> } | null;
  logger?: Logger | null;
  /** The time source, so the pacing and the ceilings are testable. */
  now?: () => Date;
  /** The deployment's default ceiling. */
  hourlyLimit?: number;
}

/** One attempt's outcome, kept briefly after the transport forgot it. */
interface FinishedLink {
  link: Link;
  forgetAt: number;
}

/** An empty link, as Go's zero value. */
function emptyLink(): Link {
  return {
    id: "",
    numberID: NilUUID,
    workspaceID: NilUUID,
    state: "",
    code: "",
    pairCode: "",
    phone: "",
    expiresAt: new Date(0),
    reason: "",
  };
}

/** Reports whether an attempt has ended, either way. */
function terminalLinkState(state: string): boolean {
  return state === LinkPaired || state === LinkExpired || state === LinkFailed;
}

/** Reports whether a status is one this module uses. */
function knownStatus(status: string): boolean {
  return (
    status === StatusUnlinked ||
    status === StatusLinking ||
    status === StatusConnected ||
    status === StatusNeedsRelink ||
    status === StatusDisconnected
  );
}

/** Reduces a typed number to the digits WhatsApp wants. */
export function normalizePhone(phone: string): string {
  return phone.replace(/[^0-9]/g, "");
}

/** Hides all but the last four digits. */
export function maskPhone(phone: string): string {
  const trimmed = phone.trim();
  if (trimmed.length <= 4) {
    return "•".repeat(trimmed.length);
  }
  return "•".repeat(trimmed.length - 4) + trimmed.slice(trimmed.length - 4);
}

/** The WhatsApp implementation. */
export class Service implements ServicePort {
  private readonly deps: Deps;
  private readonly now: () => Date;
  /** The transport can be installed after construction. */
  private transportValue: Transport | null;
  /** Remembers how a link attempt ended, so a late poll gets the answer. */
  private readonly finished = new Map<string, FinishedLink>();

  constructor(deps: Deps) {
    this.deps = {
      ...deps,
      now: deps.now ?? (() => new Date()),
      logger: deps.logger ?? null,
      hourlyLimit: deps.hourlyLimit && deps.hourlyLimit > 0 ? deps.hourlyLimit : DefaultHourlyLimit,
    };
    this.now = this.deps.now as () => Date;
    this.transportValue = deps.transport ?? null;
  }

  /** Installs the transport a service sends through, for the gateway process. */
  useTransport(transport: Transport): void {
    this.transportValue = transport;
  }

  /** The transport a call runs against. */
  private transport(): Transport | null {
    return this.transportValue;
  }

  /** The dependencies the message half reads. */
  private messageDeps(): MessageDeps {
    return {
      repository: this.deps.repository,
      watchers: this.deps.watchers ?? null,
      replies: this.deps.replies ?? null,
      logger: this.deps.logger ?? null,
      alerter: this.deps.alerter ?? null,
      hourlyLimit: this.deps.hourlyLimit as number,
      now: this.now,
      emit: (scope, kind, number) => this.emit(scope, kind, number),
    };
  }

  // ------------------------------------------------------- what a person does

  /** Returns the workspace's numbers. */
  async numbers(scope: Scope): Promise<Number[]> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return this.deps.repository.list(scope);
  }

  /** Returns one number. */
  async number(scope: Scope, id: string): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return this.deps.repository.get(scope, id);
  }

  /**
   * Records one number and who answers it, before anything is linked.
   *
   * It is separate from `beginLink` because two things have to happen in order:
   * the warning is shown and accepted against a row, and only then does the
   * link start.
   */
  async prepare(scope: Scope, request: LinkRequest): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    if (!request.agentID || request.agentID === NilUUID) {
      throw wrap(ErrInvalidInput, "pilih dulu Bolu yang menjawab nomor ini");
    }

    const number = await this.numberForLink(scope, request);
    await this.audit(scope, "whatsapp.prepared", number);
    return number;
  }

  /** Returns one message. */
  async messageByID(scope: Scope, id: string): Promise<Inbound> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return this.deps.repository.messageByID(scope, id);
  }

  /** Reads one stored attachment. */
  async mediaBytes(key: string): Promise<Uint8Array> {
    if (key.trim() === "") throw wrap(ErrInvalidInput, "tidak ada lampiran");
    if (!this.deps.media) throw wrap(ErrNotConfigured, "no media store is configured");
    return this.deps.media.get(key);
  }

  /**
   * Starts linking one number.
   *
   * It refuses until the workspace has accepted the warning and a Bolu has been
   * chosen: linking over an unsupported route is a risk somebody accepts, and a
   * number with no Bolu has nobody to answer the customers who write to it.
   */
  async beginLink(scope: Scope, request: LinkRequest): Promise<Link> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    const transport = this.transport();
    if (!transport) throw wrap(ErrNotConfigured, "no gateway is reachable");
    if (!request.agentID || request.agentID === NilUUID) {
      throw wrap(ErrInvalidInput, "pilih dulu Bolu yang menjawab nomor ini");
    }

    const number = await this.numberForLink(scope, request);
    if (numberLinked(number)) throw wrap(ErrAlreadyLinked, "nomor ini sudah tertaut");
    if (number.tosAcceptedAt === null) throw ErrTermsNotAccepted;

    const link = await transport.link(scope, number.id, number.phone);

    // The row says a link is in progress, which is what the screen reads before
    // the first code arrives.
    await this.deps.repository.setStatus(scope, number.id, StatusLinking, "");

    this.remind(link, number);
    return link;
  }

  /**
   * Finds or prepares the row this attempt works on.
   *
   * A number is created once, before it is linked, because the warning has to
   * be accepted against something and the Bolu is chosen at that moment.
   */
  private async numberForLink(scope: Scope, request: LinkRequest): Promise<Number> {
    const phone = normalizePhone(request.phone);
    if (phone.length < 8) {
      throw wrap(ErrInvalidInput, `${JSON.stringify(request.phone)} bukan nomor telepon yang masuk akal`);
    }
    // A number written the local way starts with a zero, and WhatsApp needs the
    // country-coded form: pairing with "0812…" is refused by the server with an
    // error nobody can act on.
    if (phone.startsWith("0")) {
      throw wrap(ErrInvalidInput, "tulis nomornya dengan kode negara, contoh 62812… bukan 0812…");
    }

    try {
      return await this.deps.repository.byPhone(scope, phone);
    } catch (err) {
      if (!is(err, ErrNotFound)) throw err;
    }

    const label = request.label.trim() === "" ? `WhatsApp ${maskPhone(phone)}` : request.label.trim();

    return this.deps.repository.create(scope, {
      id: NilUUID,
      workspaceID: scope.workspaceId,
      integrationID: NilUUID,
      phone,
      label,
      agentID: request.agentID,
      status: StatusUnlinked,
      statusReason: "",
      tosAcceptedAt: null,
      hourlyLimit: this.deps.hourlyLimit as number,
      connectedAt: null,
      lastSeenAt: null,
      createdAt: new Date(0),
      updatedAt: new Date(0),
    });
  }

  /** Records that the workspace accepted the warning. */
  async acceptTerms(scope: Scope, id: string): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

    const number = await this.deps.repository.acceptTerms(scope, id);
    await this.audit(scope, "whatsapp.terms_accepted", number);
    return number;
  }

  /**
   * Reports where an attempt stands, and finishes the link when the transport
   * says the phone paired.
   */
  async linkState(scope: Scope, linkID: string): Promise<Link> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    const transport = this.transport();
    if (!transport) throw wrap(ErrNotConfigured, "no gateway is reachable");

    // An attempt whose *outcome* is already known answers from here: the
    // transport forgets an attempt the moment it ends, and a screen that polls
    // one step behind must still be told what happened rather than asked to
    // start over. Only a terminal state is answered from memory.
    const remembered = this.finished.get(linkID);
    if (remembered && terminalLinkState(remembered.link.state)) {
      return remembered.link;
    }

    let link: Link;
    try {
      link = await transport.linkState(linkID);
    } catch (err) {
      if (is(err, ErrLinkExpired)) {
        return this.expired(linkID, "penautan ini sudah kedaluwarsa; minta kode baru");
      }
      throw err;
    }

    // The transport answers with the workspace it was started for, so an attempt
    // that belongs to another tenant is refused rather than reported.
    if (link.workspaceID && link.workspaceID !== NilUUID && link.workspaceID !== scope.workspaceId) {
      throw wrap(ErrNotFound, "penautan itu milik workspace lain");
    }

    if (link.state === LinkExpired || link.state === LinkFailed) {
      // The row goes back to what it was: a link that did not happen is not a
      // number that is broken.
      if (link.numberID && link.numberID !== NilUUID) {
        try {
          await this.deps.repository.setStatus(scope, link.numberID, StatusUnlinked, link.reason);
        } catch (err) {
          this.deps.logger?.warn({ err }, "could not clear a failed link's status");
        }
      }
      this.remember({ ...emptyLink(), id: linkID, state: link.state, reason: link.reason });
      return link;
    }

    this.remember(link);
    return link;
  }

  /** Asks for the phone-code alternative to scanning. */
  async pairCode(scope: Scope, linkID: string, phone: string): Promise<Link> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    const transport = this.transport();
    if (!transport) throw wrap(ErrNotConfigured, "no gateway is reachable");

    const link = await transport.pairCode(linkID, phone);
    this.remember(link);
    return link;
  }

  /** Abandons an attempt. */
  async cancelLink(scope: Scope, linkID: string): Promise<void> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    const transport = this.transport();
    if (!transport) return;

    await transport.cancel(linkID);

    // The row is left unlinked rather than failed: cancelling is a person
    // changing their mind, and the next attempt starts from the same row.
    const remembered = this.forget(linkID);
    if (remembered && remembered.numberID !== NilUUID) {
      await this.deps.repository.setStatus(scope, remembered.numberID, StatusUnlinked, "");
    }
  }

  /** What the transport calls when a phone finished pairing. */
  async paired(
    scope: Scope,
    paired: { numberID: string; phone: string; jid: string },
  ): Promise<void> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

    let number = await this.deps.repository.get(scope, paired.numberID);

    let phone = normalizePhone(paired.phone);
    if (phone === "") {
      phone = number.phone;
    }

    // The number WhatsApp reported is written onto the row, because it is the
    // number this session answers as. What the person typed was a hint.
    if (phone !== number.phone) {
      await this.deps.repository.setPhone(scope, number.id, phone);
      number = { ...number, phone };
    }

    number = await this.linkConnection(scope, number, phone);
    number = await this.deps.repository.setStatus(scope, number.id, StatusConnected, "");

    await this.deps.repository.publish(scope, number.id, true);

    await this.audit(scope, "whatsapp.linked", number);
    this.deps.logger?.info(
      { number: number.id, phone: maskPhone(number.phone) },
      "a whatsapp number is linked",
    );
  }

  /**
   * Records the number as a connection in the hub, creating it the first time
   * and re-labelling it afterwards.
   */
  private async linkConnection(scope: Scope, number: Number, phone: string): Promise<Number> {
    const connections = this.deps.connections;
    if (!connections) return number;

    const integrationID = await connections.link(scope, {
      app: App,
      accountLabel: maskPhone(phone),
      handle: number.id,
    });

    // The integration id goes in through its own statement, because it is a
    // link the hub owns rather than a field a person edits.
    await this.deps.repository.setIntegration(scope, number.id, integrationID);
    return { ...number, integrationID };
  }

  /** Chooses which Bolu answers this number. */
  async assignAgent(scope: Scope, id: string, agentID: string): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

    const number = await this.deps.repository.get(scope, id);
    const updated = await this.deps.repository.update(scope, { ...number, agentID });

    await this.audit(scope, "whatsapp.agent_assigned", updated);
    return updated;
  }

  /** Changes the number's hourly ceiling. */
  async setLimit(scope: Scope, id: string, limit: number): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    if (limit < 0 || limit > 500) {
      throw wrap(ErrInvalidInput, "batas per jam harus antara 0 dan 500");
    }

    const number = await this.deps.repository.get(scope, id);
    const updated = await this.deps.repository.update(scope, { ...number, hourlyLimit: limit });

    await this.audit(scope, "whatsapp.limit_changed", updated);
    return updated;
  }

  /** Renames the number as the workspace refers to it. */
  async setLabel(scope: Scope, id: string, label: string): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

    const trimmed = label.trim();
    if (trimmed === "") throw wrap(ErrInvalidInput, "nama nomor tidak boleh kosong");

    const number = await this.deps.repository.get(scope, id);
    const updated = await this.deps.repository.update(scope, { ...number, label: trimmed });

    await this.audit(scope, "whatsapp.renamed", updated);
    return updated;
  }

  /**
   * Ends the session and clears what was stored.
   *
   * The message history stays: what a customer wrote is not the session's, and
   * losing it because a phone was replaced would erase the record.
   */
  async unlink(scope: Scope, id: string): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");

    const number = await this.deps.repository.get(scope, id);

    const transport = this.transport();
    if (transport) {
      try {
        await transport.unlink({
          numberID: number.id,
          workspaceID: scope.workspaceId,
          phone: number.phone,
          session: null,
        });
      } catch (err) {
        if (!is(err, ErrNotConnected)) throw err;
      }
    }

    try {
      await this.deps.sessions.forgetSession(scope, number.id);
    } catch (err) {
      if (!is(err, ErrNotFound)) throw err;
    }
    await this.deps.repository.publish(scope, number.id, false);

    if (this.deps.connections && number.integrationID !== NilUUID) {
      await this.deps.connections.disconnect(scope, number.integrationID);
    }

    const updated = await this.deps.repository.setStatus(
      scope,
      number.id,
      StatusDisconnected,
      "dinonaktifkan",
    );

    await this.audit(scope, "whatsapp.unlinked", updated);
    return updated;
  }

  /** Records what the transport knows about a number. */
  async report(scope: Scope, request: ReportRequest): Promise<Number> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    if (!knownStatus(request.status)) {
      throw wrap(ErrInvalidInput, `unknown status ${JSON.stringify(request.status)}`);
    }

    let number = await this.deps.repository.get(scope, request.numberID);

    // A report that says nothing new does not move the row: the transport
    // reports on a timer, and rewriting the status every tick would be a write
    // per number per interval for no change.
    if (number.status === request.status && number.statusReason === request.reason) {
      if (request.status === StatusConnected) {
        await this.deps.repository.touch(scope, number.id, request.at);
        number = { ...number, lastSeenAt: request.at };
      }
      return number;
    }

    let updated = await this.deps.repository.setStatus(
      scope,
      number.id,
      request.status,
      request.reason,
    );
    if (request.status === StatusConnected) {
      await this.deps.repository.touch(scope, number.id, request.at);
      updated = { ...updated, lastSeenAt: request.at };
    }

    // Only a status a person has to act on alerts them. A number that dropped
    // and reconnected by itself is worth the live stream and not an email.
    if (request.status === StatusNeedsRelink || request.status === StatusDisconnected) {
      await this.alert(scope, updated, request.status);
    } else if (request.status === StatusConnected) {
      await this.emit(scope, "whatsapp.status", updated);
    }

    try {
      await this.deps.repository.publish(scope, updated.id, numberLinked(updated));
    } catch (err) {
      this.deps.logger?.warn({ err }, "could not publish a number's status to the fleet");
    }

    return updated;
  }

  /** Tells the workspace's owners that a number needs them. */
  private async alert(scope: Scope, number: Number, status: string): Promise<void> {
    const kind = status === StatusNeedsRelink ? AlertNeedsRelink : AlertDisconnected;

    await this.emit(scope, `whatsapp.${status}`, number);

    const alerter = this.deps.alerter;
    if (!alerter) return;

    try {
      await alerter.alert(scope, {
        kind,
        numberID: number.id,
        phone: number.phone,
        status,
        reason: number.statusReason,
        workspaceID: scope.workspaceId,
      });
    } catch (err) {
      this.deps.logger?.warn({ err }, "could not alert the owners about a number");
    }
  }

  /** Records what a person did to a number, for the trail. */
  private async audit(scope: Scope, action: string, number: Number): Promise<void> {
    await this.emit(scope, action, number);
  }

  /** Publishes one event into the workspace's live stream. */
  private async emit(scope: Scope, kind: string, number: Number): Promise<void> {
    const activity = this.deps.activity;
    if (!activity) return;

    const payload: Record<string, unknown> = {
      number_id: number.id,
      phone: maskPhone(number.phone),
      label: number.label,
      status: number.status,
    };
    if (number.statusReason !== "") payload.reason = number.statusReason;
    if (number.agentID !== NilUUID) payload.agent_id = number.agentID;

    try {
      await activity.emit({ workspaceID: scope.workspaceId, type: kind, payload });
    } catch (err) {
      this.deps.logger?.warn({ err }, "could not publish a whatsapp event");
    }
  }

  /** Keeps a link attempt's state briefly, so a late poll sees the outcome. */
  private remember(link: Link): void {
    if (link.id === "") return;

    // A sweep on every write keeps the map from growing with attempts nobody
    // asked about again.
    const now = this.now().getTime();
    for (const [id, entry] of this.finished) {
      if (entry.forgetAt < now) this.finished.delete(id);
    }

    this.finished.set(link.id, { link, forgetAt: now + linkStateTTL });
  }

  /** Removes a remembered attempt and answers it. */
  private forget(linkID: string): Link | null {
    const entry = this.finished.get(linkID);
    if (!entry) return null;
    this.finished.delete(linkID);
    return entry.link;
  }

  /** Answers with a reason a person can act on: the answer is a new code. */
  private expired(linkID: string, reason: string): Link {
    const link: Link = { ...emptyLink(), id: linkID, state: LinkExpired, reason };
    this.remember(link);
    return link;
  }

  /** Records an attempt's first state so an early poll answers with it. */
  private remind(link: Link, number: Number): void {
    this.remember({
      ...link,
      numberID: number.id,
      workspaceID: number.workspaceID,
      phone: number.phone,
    });
  }

  // -------------------------------------------------------- what the gateway does
  // These delegate to messages.ts, which is the same Go package's other file.

  /** Records one inbound message and answers whether it was new. */
  async receive(
    scope: Scope,
    numberID: string,
    message: Inbound,
  ): Promise<{ message: Inbound; created: boolean }> {
    return receiveMessage(this.messageDeps(), scope, numberID, message);
  }

  /** Returns the messages of one number that may go out now. */
  async claim(scope: Scope, id: string, limit: number): Promise<Claimed> {
    return claimMessages(this.messageDeps(), scope, id, limit);
  }

  /** Records one message to send. */
  async enqueue(scope: Scope, request: EnqueueRequest): Promise<Outbox> {
    return enqueueMessage(this.messageDeps(), scope, request);
  }

  /** Records that a message left, with WhatsApp's own id for it. */
  async sent(scope: Scope, id: string, waMessageID: string, at: Date): Promise<void> {
    await sentMessage(this.messageDeps(), scope, id, waMessageID, at);
  }

  /** Puts a message back in the queue with a later time. */
  async retry(scope: Scope, id: string, reason: string, at: Date): Promise<void> {
    await retryMessage(this.messageDeps(), scope, id, reason, at);
  }

  /** Records that a message will not be retried, so it stays visible. */
  async failed(scope: Scope, id: string, reason: string, attempts: number): Promise<void> {
    await failedMessage(this.messageDeps(), scope, id, reason, attempts);
  }

  /** Puts messages a dead gateway left mid-send back in the queue. */
  async reclaim(scope: Scope, olderThan: Date): Promise<number> {
    return reclaimMessages(this.messageDeps(), scope, olderThan);
  }

  /** Returns the messages whose bytes have not been fetched. */
  async pendingMedia(scope: Scope, limit: number): Promise<Inbound[]> {
    return pendingMediaMessages(this.messageDeps(), scope, limit);
  }

  /** Records where a message's bytes were stored, or that they could not be. */
  async storeMedia(scope: Scope, id: string, media: Media): Promise<void> {
    await storeMediaMessage(this.messageDeps(), scope, id, media);
  }

  /** Returns what customers wrote, newest first. */
  async messages(scope: Scope, filter: MessageFilter): Promise<Inbound[]> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return listMessages(this.messageDeps(), scope, filter);
  }

  /** Returns what this number has been asked to send, newest first. */
  async outbox(scope: Scope, filter: OutboxFilter): Promise<Outbox[]> {
    if (scopeIsZero(scope)) throw wrap(ErrInvalidInput, "workspace is required");
    return listOutbox(this.messageDeps(), scope, filter);
  }

  /** Answers which number a connection speaks as. */
  async numberByIntegration(scope: Scope, integrationID: string): Promise<string> {
    return numberByIntegrationID(this.messageDeps(), scope, integrationID);
  }

  /** Returns one conversation oldest first, for the connection a call names. */
  async history(scope: Scope, integrationID: string, chatJID: string, limit: number): Promise<Inbound[]> {
    return historyMessages(this.messageDeps(), scope, integrationID, chatJID, limit);
  }
}

export { ErrNoAgent };
