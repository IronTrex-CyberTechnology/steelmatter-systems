/** Bounded newline framing: reject oversized input while it is arriving. */
export const MAX_LINE_BYTES = 16 << 20;

export class LineFramer {
  readonly #onLine: (line: string) => void;
  readonly #onError: (kind: 'size' | 'encoding') => void;
  readonly #limit: number;
  #buffer: Buffer = Buffer.alloc(0);
  #size = 0;
  #discarding = false;

  constructor(onLine: (line: string) => void, onError: (kind: 'size' | 'encoding') => void, limit = MAX_LINE_BYTES) {
    if (!Number.isSafeInteger(limit) || limit < 1) throw new RangeError('Invalid line limit');
    this.#onLine = onLine;
    this.#onError = onError;
    this.#limit = limit;
  }

  push(chunk: Buffer): void {
    let start = 0;
    while (start < chunk.length) {
      const newline = chunk.indexOf(10, start);
      const end = newline < 0 ? chunk.length : newline;
      if (!this.#discarding) {
        const length = end - start;
        if (this.#size + length > this.#limit) {
          this.#buffer = Buffer.alloc(0);
          this.#size = 0;
          this.#discarding = true;
          this.#onError('size');
        } else if (length > 0) {
          // Bound storage even if input arrives in millions of tiny chunks.
          // Never retain the caller's potentially oversized backing buffer.
          const required = this.#size + length;
          if (required > this.#buffer.length) {
            const next = Buffer.alloc(Math.min(this.#limit, Math.max(required, 1024, this.#buffer.length * 2)));
            this.#buffer.copy(next, 0, 0, this.#size);
            this.#buffer = next;
          }
          chunk.copy(this.#buffer, this.#size, start, end);
          this.#size += length;
        }
      }
      if (newline < 0) return;
      if (!this.#discarding) this.#emit();
      this.#discarding = false;
      start = newline + 1;
    }
  }

  end(): void {
    if (!this.#discarding && this.#size > 0) this.#emit();
    this.#buffer = Buffer.alloc(0);
    this.#size = 0;
    this.#discarding = false;
  }

  #emit(): void {
    let bytes = this.#buffer.subarray(0, this.#size);
    this.#buffer = Buffer.alloc(0);
    this.#size = 0;
    if (bytes.at(-1) === 13) bytes = bytes.subarray(0, -1);
    let line: string;
    try {
      // Preserve a BOM so JSON.parse rejects it; never repair invalid UTF-8.
      line = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true }).decode(bytes);
    } catch {
      this.#onError('encoding');
      return;
    }
    this.#onLine(line);
  }
}
