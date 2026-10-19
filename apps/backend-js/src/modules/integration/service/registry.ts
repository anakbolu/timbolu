/**
 * The registry holds every adapter this deployment knows.
 *
 * Registration happens once, while the container is being assembled, so the
 * lock is not for concurrent use: it is there so a read from a task goroutine
 * cannot race a late registration during a test. JavaScript has one thread, so
 * the map is the whole of it.
 *
 * Ported 1:1 from internal/modules/integration/service/registry.go.
 */

import {
  ErrAdapterExists,
  ErrInvalidSpec,
  type Adapter,
  type Registry,
  type ToolSpec,
  type TriggerSpec,
  validateToolSpec,
  validateTriggerSpec,
  toolSpecApp,
} from "../domain/adapter.ts";

export class AdapterRegistry implements Registry {
  private readonly adapters = new Map<string, Adapter>();

  /**
   * Adds an adapter.
   *
   * Every spec it declares is checked here, so an adapter with a typo in a
   * label or a name that is not `app.action` fails at startup rather than at
   * the first call the model makes.
   */
  register(adapter: Adapter): void {
    if (!adapter) {
      throw new Error(`${ErrInvalidSpec.message}: a nil adapter`);
    }

    const app = adapter.app().trim();
    if (app === "") {
      throw new Error(`${ErrInvalidSpec.message}: an adapter needs an app key`);
    }

    for (const spec of adapter.tools()) {
      const failure = validateToolSpec(spec);
      if (failure) {
        throw failure;
      }
      // A tool must belong to the app that declares it: `gmail.search` from the
      // jira adapter would be offered under a grant that does not cover it.
      if (toolSpecApp(spec) !== app) {
        throw new Error(`${ErrInvalidSpec.message}: ${JSON.stringify(app)} declares ${JSON.stringify(spec.name)}`);
      }
    }
    for (const trigger of adapter.triggers()) {
      const failure = validateTriggerSpec(trigger);
      if (failure) {
        throw failure;
      }
      if (trigger.app !== app) {
        throw new Error(
          `${ErrInvalidSpec.message}: ${JSON.stringify(app)} declares the trigger ${JSON.stringify(trigger.event)}`,
        );
      }
    }

    if (this.adapters.has(app)) {
      throw new Error(`${ErrAdapterExists.message}: ${JSON.stringify(app)}`);
    }
    this.adapters.set(app, adapter);
  }

  /** The registered application keys, sorted. */
  apps(): string[] {
    return Array.from(this.adapters.keys()).sort();
  }

  /** The adapter for an application. */
  adapter(app: string): Adapter | null {
    return this.adapters.get(app) ?? null;
  }

  /** Every tool of every adapter, sorted by name. */
  tools(): ToolSpec[] {
    const tools: ToolSpec[] = [];
    for (const adapter of this.adapters.values()) {
      tools.push(...adapter.tools());
    }
    return tools.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /** One application's tools, sorted by name. */
  toolsFor(app: string): ToolSpec[] {
    const adapter = this.adapters.get(app);
    if (!adapter) {
      return [];
    }
    return adapter.tools().sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /** Every trigger of every adapter, sorted. */
  triggers(): TriggerSpec[] {
    const triggers: TriggerSpec[] = [];
    for (const adapter of this.adapters.values()) {
      triggers.push(...adapter.triggers());
    }
    return triggers.sort((a, b) => (a.event < b.event ? -1 : a.event > b.event ? 1 : 0));
  }

  /** Finds a tool by its name. */
  lookup(name: string): { spec: ToolSpec; adapter: Adapter } | null {
    const cut = name.indexOf(".");
    if (cut < 0) {
      return null;
    }

    const adapter = this.adapters.get(name.slice(0, cut));
    if (!adapter) {
      return null;
    }

    for (const spec of adapter.tools()) {
      if (spec.name === name) {
        return { spec, adapter };
      }
    }
    return null;
  }
}

/** Builds an empty registry. */
export function newRegistry(): AdapterRegistry {
  return new AdapterRegistry();
}
