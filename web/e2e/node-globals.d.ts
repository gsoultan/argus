/**
 * The handful of Node globals this suite uses, declared rather than pulled in
 * with @types/node.
 *
 * The specs are TypeScript and are now typechecked — a syntax error in
 * helpers.ts once passed `bun run typecheck` untouched and surfaced only as
 * Playwright failing to collect any tests at all. What they need from Node is
 * one environment variable and a byte buffer for the generated RDP stream;
 * adding Node's whole type surface to describe that would be a large dependency
 * for a small purpose, and vite.config.ts already takes the same approach.
 */

declare const process: { env: Record<string, string | undefined> }

interface Buffer extends Uint8Array {
  writeUInt8(value: number, offset: number): number
  writeUInt16LE(value: number, offset: number): number
  writeUInt32LE(value: number, offset: number): number
}

declare const Buffer: {
  alloc(size: number): Buffer
  concat(list: readonly Buffer[]): Buffer
  from(data: ArrayLike<number> | ArrayBufferLike | string): Buffer
}
