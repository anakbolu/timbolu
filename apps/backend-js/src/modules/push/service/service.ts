/**
 * The push use case: turn one activity event into notifications for the phones
 * the account registered.
 *
 * The service owns three decisions and nothing else. It reads tokens through the
 * device module rather than a query of its own, because that registry is the
 * only place that knows an address is live. It drops a row that is not an Expo
 * address before sending, so one bad registration cannot take the whole batch
 * down. And it prunes a token Expo reports as `DeviceNotRegistered`, because a
 * token that points at an uninstalled app would otherwise be retried on every
 * event forever.
 *
 * Every dependency is a port, so the tests never open a database or the network.
 * A push is best-effort by design: a failure here is logged and answered with
 * null, never raised into the task or approval that emitted the event.
 */

import type { Logger } from "../../../platform/logger.ts";
import type { Device } from "../../device/domain/device.ts";
import {
  notificationFor,
  type Event,
  type Notification,
  type Notifier,
  type Scope,
} from "../domain/push.ts";
import type { Message, Result } from "../expo/client.ts";

/**
 * The slice of the device registry a push needs. Narrower than the device
 * module's own Repository so a test can fake only the two calls used here.
 */
export interface DeviceRegistry {
  /** Every token the account registered. */
  listTokens(scope: Scope): Promise<Device[]>;
  /** Removes one of the account's tokens. */
  remove(scope: Scope, token: string): Promise<void>;
}

/** The Expo sender. Narrowed to one method for the same reason. */
export interface Sender {
  send(messages: Message[]): Promise<Result>;
}

/** The service dependencies. */
export interface Deps {
  devices: DeviceRegistry;
  expo: Sender;
  logger?: Logger | null;
}

/**
 * The two address shapes Expo issues. Anything else is a stored value that is
 * not a push address, and sending it would make Expo reject the whole request.
 */
const ExpoToken = /^(?:Exponent|Expo)PushToken\[[^\]\s]+\]$/;

/** The push implementation. */
export class Service implements Notifier {
  constructor(private readonly deps: Deps) {}

  /**
   * Notifies every phone the event's account registered. Answers null when
   * there is nothing to send, so a caller can tell "not mapped" and "no
   * devices" from "Expo accepted some".
   */
  async send(scope: Scope, event: Event): Promise<Result | null> {
    const notification = notificationFor(event);
    if (!notification) {
      return null;
    }

    // A token belongs to a user, not to a workspace. Without one there is no
    // registry to read, and no safe way to guess whose phones these are.
    const userId = (scope.userId ?? "").trim();
    if (userId === "") {
      return null;
    }

    const owner: Scope = { ...scope, userId };

    let devices: Device[];
    try {
      devices = await this.deps.devices.listTokens(owner);
    } catch (err) {
      this.deps.logger?.warn({ err, type: event.type }, "could not read push tokens");
      return null;
    }

    const tokens = usableTokens(devices);
    if (tokens.length === 0) {
      return null;
    }

    let result: Result;
    try {
      result = await this.deps.expo.send(
        tokens.map((token) => messageFor(token, notification)),
      );
    } catch (err) {
      this.deps.logger?.warn({ err, type: event.type }, "push notification was not sent");
      return null;
    }

    await this.prune(owner, result.unregistered);
    return result;
  }

  /** Deletes the addresses Expo says no longer exist, one at a time. */
  private async prune(scope: Scope, tokens: string[]): Promise<void> {
    for (const token of tokens) {
      try {
        await this.deps.devices.remove(scope, token);
      } catch (err) {
        this.deps.logger?.warn({ err }, "could not remove an unregistered push token");
      }
    }
  }
}

/**
 * The kill switch's answer: a notifier that reads nothing and sends nothing.
 *
 * It is a real notifier rather than null so every caller keeps the same
 * optional port and the switch stays invisible below the container: a phone
 * registry entry that nobody may send to looks exactly like an account with no
 * phones.
 */
export class Disabled implements Notifier {
  async send(_scope: Scope, _event: Event): Promise<null> {
    return null;
  }
}

/** The dependencies, plus the deployment's switch. */
export interface FactoryDeps extends Deps {
  enabled: boolean;
}

/**
 * Builds the notifier a deployment should call.
 *
 * With `enabled` false the Expo client and the device registry are never
 * touched, so no request can reach exp.host even when a token is registered.
 */
export function buildNotifier(deps: FactoryDeps): Notifier {
  return deps.enabled ? new Service(deps) : new Disabled();
}

/**
 * The addresses worth sending to: trimmed, shaped like an Expo token, and
 * unique. A token that fails the shape check is skipped rather than repaired —
 * there is no way to guess what the phone meant to register.
 */
function usableTokens(devices: Device[]): string[] {
  const seen = new Set<string>();
  const tokens: string[] = [];
  for (const device of devices) {
    const token = device.token.trim();
    if (!ExpoToken.test(token) || seen.has(token)) {
      continue;
    }
    seen.add(token);
    tokens.push(token);
  }
  return tokens;
}

/** Builds the Expo message a notification becomes. */
function messageFor(token: string, notification: Notification): Message {
  return {
    to: token,
    title: notification.title,
    body: notification.body,
    sound: "default",
    data: { deepLink: notification.deepLink },
  };
}
