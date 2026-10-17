/**
 * Object storage for the files the application writes.
 *
 * Ported from packages/platform/storage/storage.go. Two drivers exist: an
 * S3-compatible one for anything deployed, and a local directory for
 * development and tests.
 */

import { createHash } from "node:crypto";
import { mkdir, readFile, readdir, rm, rmdir, stat, writeFile } from "node:fs/promises";
import { dirname, join, relative, resolve, sep } from "node:path";

/** No storage driver was configured. */
export const ErrNotConfigured = new Error("storage: not configured");
/** The key does not exist. */
export const ErrObjectNotFound = new Error("storage: object not found");
/** A key would resolve outside the storage root. */
export const ErrKeyEscapesRoot = new Error("storage: key escapes the storage root");

/** One stored file. */
export interface Object {
  key: string;
  contentType: string;
  byteSize: number;
  /** Hex SHA-256 of the content, which lets a caller verify what it stored. */
  checksumSha256: string;
}

/** Writes and reads objects by key. */
export interface Store {
  put(key: string, contentType: string, content: Uint8Array): Promise<Object>;
  get(key: string): Promise<Uint8Array>;
  /** Removing something absent is not an error. */
  delete(key: string): Promise<void>;
  /** Every key under a prefix. */
  list(prefix: string): Promise<string[]>;
  /** Names the implementation, for logs and the readiness probe. */
  driver(): string;
}

/** The hex SHA-256 of content. */
export function digest(content: Uint8Array): string {
  return createHash("sha256").update(content).digest("hex");
}

/** Keeps objects as files under a root directory. */
export class LocalStore implements Store {
  private readonly root: string;

  private constructor(root: string) {
    this.root = root;
  }

  /** Builds a local store, creating the root when it does not exist. */
  static async create(root: string): Promise<LocalStore> {
    if (root.trim() === "") {
      throw new Error(`${ErrNotConfigured.message}: root directory is required`);
    }

    try {
      await mkdir(root, { recursive: true, mode: 0o755 });
    } catch (err) {
      throw new Error(`storage: create root ${root}: ${(err as Error).message}`);
    }

    return new LocalStore(resolve(root));
  }

  driver(): string {
    return "local";
  }

  async put(key: string, contentType: string, content: Uint8Array): Promise<Object> {
    const path = this.path(key);
    try {
      await mkdir(dirname(path), { recursive: true, mode: 0o755 });
    } catch (err) {
      throw new Error(`storage: create directory: ${(err as Error).message}`);
    }

    try {
      await writeFile(path, content, { mode: 0o644 });
    } catch (err) {
      throw new Error(`storage: write ${key}: ${(err as Error).message}`);
    }

    return {
      key,
      contentType,
      byteSize: content.length,
      checksumSha256: digest(content),
    };
  }

  async get(key: string): Promise<Uint8Array> {
    const path = this.path(key);
    try {
      return await readFile(path);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") {
        throw new Error(`${ErrObjectNotFound.message}: ${key}`);
      }
      throw new Error(`storage: read ${key}: ${(err as Error).message}`);
    }
  }

  /**
   * Deletes one key and prunes the directories it was the last file in, so the
   * local tree leaves the same trace as an S3 bucket: none.
   */
  async delete(key: string): Promise<void> {
    const path = this.path(key);
    try {
      await rm(path);
    } catch (err) {
      const code = (err as NodeJS.ErrnoException).code;
      if (code !== "ENOENT") {
        throw new Error(`storage: delete ${key}: ${(err as Error).message}`);
      }
    }

    await this.pruneEmptyParents(dirname(path));
  }

  private async pruneEmptyParents(directory: string): Promise<void> {
    let current = directory;
    while (current !== this.root && current.startsWith(this.root + sep)) {
      // rmdir is atomic and fails when the directory is not empty, which is the
      // check itself: listing first and then removing would race a write.
      try {
        await rmdir(current);
      } catch {
        return;
      }
      current = dirname(current);
    }
  }

  async list(prefix: string): Promise<string[]> {
    const path = this.path(prefix.replace(/\/$/, ""));
    const keys: string[] = [];

    let info;
    try {
      info = await stat(path);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") {
        // A prefix nothing was ever stored under is an empty listing.
        return keys;
      }
      throw new Error(`storage: list ${prefix}: ${(err as Error).message}`);
    }

    if (!info.isDirectory()) {
      keys.push(relative(this.root, path).split(sep).join("/"));
      return keys;
    }

    const walk = async (current: string): Promise<void> => {
      const entries = await readdir(current, { withFileTypes: true });
      for (const entry of entries) {
        const full = join(current, entry.name);
        if (entry.isDirectory()) {
          await walk(full);
        } else {
          keys.push(relative(this.root, full).split(sep).join("/"));
        }
      }
    };

    try {
      await walk(path);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") {
        return keys;
      }
      throw new Error(`storage: list ${prefix}: ${(err as Error).message}`);
    }

    return keys;
  }

  /** Resolves a key under the root and refuses anything that lands outside. */
  private path(key: string): string {
    const cleaned = key.trim().replace(/^\/+/, "");
    if (cleaned === "") {
      throw new Error(`${ErrKeyEscapesRoot.message}: empty key`);
    }

    const resolved = resolve(this.root, cleaned);
    if (resolved !== this.root && !resolved.startsWith(this.root + sep)) {
      throw new Error(`${ErrKeyEscapesRoot.message}: ${key}`);
    }
    return resolved;
  }
}

/**
 * Reads a reader with a cap, which is how an upload is bounded before it is
 * buffered.
 */
export async function readAll(reader: ReadableStream<Uint8Array> | AsyncIterable<Uint8Array>, limit: number): Promise<Uint8Array> {
  if (limit <= 0) {
    throw new Error("storage: limit must be positive");
  }

  const chunks: Uint8Array[] = [];
  let written = 0;

  for await (const chunk of reader as AsyncIterable<Uint8Array>) {
    chunks.push(chunk);
    written += chunk.length;
    if (written > limit) {
      throw new Error(`storage: upload exceeds ${limit} bytes`);
    }
  }

  const out = new Uint8Array(written);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}
