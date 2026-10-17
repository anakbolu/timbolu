/**
 * Server-Sent Events, parsed the way an LLM provider emits them.
 *
 * Ported from internal/platform/llm/sse/sse.go. A naive line scanner breaks on
 * the two things that happen in production: a JSON payload split across two
 * reads, and a stream that ends without its terminating event. The reader below
 * handles both, and both adapters share it so their streaming behaviour cannot
 * drift.
 */

/** One Server-Sent Event. */
export interface Event {
  /** The `event:` field, empty when the provider only uses `data:`. */
  name: string;
  /** The concatenated `data:` payload; multi-line data is joined with newlines. */
  data: string;
  /** The optional `id:` field. */
  id: string;
}

/** Comments to ignore: providers send keep-alives as comments. */
const COMMENT_PREFIX = ":";

/** The largest single event a provider sends (a full tool-call argument block). */
const MAX_EVENT_BYTES = 4 * 1024 * 1024;

/** A stream that produced a line longer than the reader accepts. */
export class LineTooLongError extends Error {
  constructor() {
    super("sse: event is too long");
    this.name = "LineTooLongError";
  }
}

/**
 * Decodes a byte stream into lines, buffering across chunk boundaries. Mirrors
 * bufio.Scanner with ScanLines: `\n` terminates a line and a trailing `\r` is
 * dropped, so CRLF streams read the same as LF ones.
 */
class LineDecoder {
  private readonly decoder = new TextDecoder();
  private buffer = "";
  private done = false;

  constructor(private readonly source: AsyncIterable<Uint8Array>) {}

  async next(): Promise<string | null> {
    for (;;) {
      const newline = this.buffer.indexOf("\n");
      if (newline >= 0) {
        let line = this.buffer.slice(0, newline);
        this.buffer = this.buffer.slice(newline + 1);
        if (line.endsWith("\r")) line = line.slice(0, -1);
        return line;
      }
      if (this.done) {
        // The stream ended mid-line: bufio.Scanner delivers what is buffered.
        if (this.buffer.length > 0) {
          let line = this.buffer;
          this.buffer = "";
          if (line.endsWith("\r")) line = line.slice(0, -1);
          return line;
        }
        return null;
      }
      const chunk = await this.readChunk();
      if (chunk === null) {
        this.done = true;
        continue;
      }
      this.buffer += this.decoder.decode(chunk, { stream: true });
      if (this.buffer.length > MAX_EVENT_BYTES) throw new LineTooLongError();
    }
  }

  private pending: AsyncIterator<Uint8Array> | null = null;

  private async readChunk(): Promise<Uint8Array | null> {
    this.pending ??= this.source[Symbol.asyncIterator]();
    const step = await this.pending.next();
    return step.done ? null : step.value;
  }
}

/**
 * Turns a byte stream into events.
 *
 * `next` returns the next event, or null when the stream ends — whether or not
 * the provider sent its [DONE] marker. A read failure throws.
 */
export class Reader {
  private readonly lines: LineDecoder;

  constructor(source: AsyncIterable<Uint8Array> | ReadableStream<Uint8Array>) {
    this.lines = new LineDecoder(toAsyncIterable(source));
  }

  async next(): Promise<Event | null> {
    const event: Event = { name: "", data: "", id: "" };
    const data: string[] = [];

    for (;;) {
      const line = await this.lines.next();
      if (line === null) {
        // The stream ended. Anything buffered is the last event, which is what
        // happens when a provider closes without a trailing blank line.
        if (event.name !== "" || data.length > 0) {
          event.data = data.join("\n");
          return event;
        }
        return null;
      }

      // A blank line terminates the event.
      if (line.trim() === "") {
        if (event.name === "" && data.length === 0) {
          // Consecutive blank lines are allowed; keep reading.
          continue;
        }
        event.data = data.join("\n");
        return event;
      }

      if (line.startsWith(COMMENT_PREFIX)) continue;

      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? "" : line.slice(colon + 1);
      // A single space after the colon is framing, not part of the value.
      if (value.startsWith(" ")) value = value.slice(1);

      switch (field) {
        case "event":
          event.name = value;
          break;
        case "data":
          data.push(value);
          break;
        case "id":
          event.id = value;
          break;
        case "retry":
          // Not used: the providers we talk to do not ask for a retry delay.
          break;
      }
    }
  }

  /** The reader as an async iterable, which is what callers normally want. */
  async *events(): AsyncIterable<Event> {
    for (;;) {
      const event = await this.next();
      if (event === null) return;
      yield event;
    }
  }
}

function toAsyncIterable(
  source: AsyncIterable<Uint8Array> | ReadableStream<Uint8Array>,
): AsyncIterable<Uint8Array> {
  if (Symbol.asyncIterator in source) return source as AsyncIterable<Uint8Array>;
  const stream = source as ReadableStream<Uint8Array>;
  return {
    [Symbol.asyncIterator]() {
      const reader = stream.getReader();
      return {
        async next() {
          const step = await reader.read();
          return step.done ? { done: true as const, value: undefined } : { done: false as const, value: step.value };
        },
        async return() {
          await reader.cancel();
          return { done: true as const, value: undefined };
        },
      };
    },
  };
}

/** Reports whether a data payload is a provider's end marker. */
export function isDone(data: string): boolean {
  return data.trim() === "[DONE]";
}

/** Feeds a payload in fixed-size chunks, so a parser can be exercised on splits. */
export function splitLines(payload: string, chunk: number): string[] {
  if (chunk <= 0) return [payload];
  const chunks: string[] = [];
  for (let start = 0; start < payload.length; start += chunk) {
    chunks.push(payload.slice(start, start + chunk));
  }
  return chunks;
}

/** Replays pre-recorded chunks, so a test can reproduce an awkward stream. */
export async function* replay(chunks: string[]): AsyncIterable<Uint8Array> {
  const encoder = new TextEncoder();
  for (const chunk of chunks) yield encoder.encode(chunk);
}

/** Keeps the bytes an adapter streamed, so a test can replay them without a network. */
export interface Recording {
  body: Uint8Array;
}

/** A byte stream over a recorded body. */
export function recordedBody(recording: Recording): ReadableStream<Uint8Array> {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(recording.body);
      controller.close();
    },
  });
}

/** Serialises one event into the wire framing. */
export function formatEvent(event: Event): string {
  let out = "";
  if (event.name) out += `event: ${event.name}\n`;
  if (event.id) out += `id: ${event.id}\n`;
  for (const line of event.data.split("\n")) out += `data: ${line}\n`;
  return out + "\n";
}

/**
 * Turns an async iterable of events into a `text/event-stream` body, which is
 * the write side the chat module needs for its own SSE responses.
 */
export function eventStreamBody(source: AsyncIterable<Event>): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  const iterator = source[Symbol.asyncIterator]();
  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      try {
        const step = await iterator.next();
        if (step.done) {
          controller.close();
          return;
        }
        controller.enqueue(encoder.encode(formatEvent(step.value)));
      } catch (err) {
        controller.error(err);
      }
    },
    async cancel() {
      await iterator.return?.(undefined);
    },
  }, { highWaterMark: 0 });
}

/** A `Response` that streams the given events as `text/event-stream`. */
export function eventStreamResponse(
  source: AsyncIterable<Event>,
  init: ResponseInit = {},
): Response {
  const headers = new Headers(init.headers as any);
  headers.set("Content-Type", "text/event-stream");
  headers.set("Cache-Control", "no-cache");
  headers.set("Connection", "keep-alive");
  return new Response(eventStreamBody(source), { ...init, headers });
}
