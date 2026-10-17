/**
 * The S3-compatible storage driver.
 *
 * Ported from packages/platform/storage/s3.go. One driver serves MinIO, R2, B2
 * and AWS S3; the choice is configuration.
 */

import { Readable } from "node:stream";
import * as Minio from "minio";
import { digest, ErrNotConfigured, ErrObjectNotFound, type Object, type Store } from "./storage";

/** One connection to an S3-compatible service. */
export interface S3Config {
  /** The host, without a scheme, e.g. "minio:9000". */
  endpoint: string;
  /** Required by AWS, ignored by most alternatives. */
  region: string;
  /** Created when it is missing. */
  bucket: string;
  accessKey: string;
  secretKey: string;
  /** Switches to https. */
  useSSL: boolean;
  /** Uses bucket-in-path addressing, which MinIO and R2 want. */
  pathStyle: boolean;
}

/** Stores objects in an S3-compatible bucket. */
export class S3Store implements Store {
  private readonly client: Minio.Client;
  private readonly bucket: string;

  private constructor(client: Minio.Client, bucket: string) {
    this.client = client;
    this.bucket = bucket;
  }

  /**
   * Connects and makes sure the bucket exists, so the API either has somewhere
   * to write or fails at startup with a clear message.
   */
  static async create(cfg: S3Config): Promise<S3Store> {
    if (cfg.endpoint.trim() === "" || cfg.bucket.trim() === "") {
      throw new Error(`${ErrNotConfigured.message}: endpoint and bucket are required`);
    }

    let client: Minio.Client;
    try {
      client = new Minio.Client({
        endPoint: cfg.endpoint,
        accessKey: cfg.accessKey,
        secretKey: cfg.secretKey,
        useSSL: cfg.useSSL,
        region: cfg.region || undefined,
        pathStyle: cfg.pathStyle,
      });
    } catch (err) {
      throw new Error(`storage: build s3 client: ${(err as Error).message}`);
    }

    let exists: boolean;
    try {
      exists = await client.bucketExists(cfg.bucket);
    } catch (err) {
      throw new Error(`storage: check bucket ${cfg.bucket}: ${(err as Error).message}`);
    }

    if (!exists) {
      try {
        await client.makeBucket(cfg.bucket, cfg.region || undefined);
      } catch (err) {
        throw new Error(`storage: create bucket ${cfg.bucket}: ${(err as Error).message}`);
      }
    }

    return new S3Store(client, cfg.bucket);
  }

  driver(): string {
    return "s3";
  }

  /**
   * Pages through the bucket under a prefix. A single call would stop at a
   * thousand objects, which for "delete everything this tenant owns" is the
   * difference between erasing a workspace and erasing part of it.
   */
  async list(prefix: string): Promise<string[]> {
    const keys: string[] = [];
    let continuation = "";

    for (;;) {
      let page = 0;
      const stream = this.client.listObjectsV2(this.bucket, prefix, true, continuation);
      try {
        for await (const object of stream) {
          if (!object.name) continue;
          keys.push(object.name);
          continuation = object.name;
          page++;
        }
      } catch (err) {
        throw new Error(`storage: list ${prefix}: ${(err as Error).message}`);
      }

      if (page === 0) {
        return keys;
      }
    }
  }

  async put(key: string, contentType: string, content: Uint8Array): Promise<Object> {
    const type = contentType.trim() === "" ? "application/octet-stream" : contentType.trim();

    try {
      await this.client.putObject(this.bucket, key, Buffer.from(content), content.length, {
        "Content-Type": type,
      });
    } catch (err) {
      throw new Error(`storage: put ${key}: ${(err as Error).message}`);
    }

    return {
      key,
      contentType: type,
      byteSize: content.length,
      checksumSha256: digest(content),
    };
  }

  async get(key: string): Promise<Uint8Array> {
    let object: Readable;
    try {
      object = (await this.client.getObject(this.bucket, key)) as Readable;
    } catch (err) {
      throw new Error(`storage: get ${key}: ${(err as Error).message}`);
    }

    // minio's client is lazy: a missing object surfaces on the first read.
    try {
      const chunks: Buffer[] = [];
      for await (const chunk of object) {
        chunks.push(Buffer.from(chunk));
      }
      return Buffer.concat(chunks);
    } catch (err) {
      if (isMissing(err)) {
        throw new Error(`${ErrObjectNotFound.message}: ${key}`);
      }
      throw new Error(`storage: read ${key}: ${(err as Error).message}`);
    } finally {
      object.destroy();
    }
  }

  async delete(key: string): Promise<void> {
    try {
      await this.client.removeObject(this.bucket, key);
    } catch (err) {
      throw new Error(`storage: delete ${key}: ${(err as Error).message}`);
    }
  }

  /**
   * Returns a URL that serves one object for a limited time, so a browser can
   * fetch a large attachment directly from storage.
   */
  async presignGet(key: string, ttlMs: number): Promise<string> {
    const expires = ttlMs > 0 ? Math.max(1, Math.floor(ttlMs / 1000)) : 15 * 60;
    try {
      return await this.client.presignedGetObject(this.bucket, key, expires);
    } catch (err) {
      throw new Error(`storage: presign ${key}: ${(err as Error).message}`);
    }
  }
}

/** True for the S3 responses that mean the object does not exist. */
function isMissing(err: unknown): boolean {
  const candidate = err as { code?: string; statusCode?: number; status?: number };
  return candidate?.code === "NoSuchKey" || candidate?.statusCode === 404 || candidate?.status === 404;
}
