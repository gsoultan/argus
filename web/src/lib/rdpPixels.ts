/**
 * The RDP bitmap decoder, compiled from internal/rdp/pixels to WebAssembly.
 *
 * Kept out of the worker so it can be loaded and tested on its own: the module
 * is a build artefact (scripts/build-wasm.sh), and a test against the committed
 * file is what notices it has fallen out of step with this glue.
 */

/** The exports documented in cmd/argus-pixels-wasm. */
interface PixelsExports {
  memory: WebAssembly.Memory
  _initialize?: () => void
  input(n: number): number
  decode(width: number, height: number, format: number): number
}

export interface PixelsModule {
  /**
   * Expands one rectangle to RGBA, or returns null if it is malformed.
   *
   * `format` is the frame header's format byte: colour depth in the low seven
   * bits, 0x80 for RLE. The result is a copy: the module reuses its buffer on
   * the next call, and createImageBitmap may read its input after this returns.
   */
  decode(
    data: Uint8Array,
    width: number,
    height: number,
    format: number,
  ): Uint8ClampedArray<ArrayBuffer> | null
}

/**
 * Instantiates the decoder.
 *
 * The imports are the five WASI calls TinyGo's runtime makes; none of them does
 * anything a decoder needs, so each is the smallest answer that satisfies it.
 */
export async function loadPixels(
  source: Response | PromiseLike<Response> | BufferSource,
): Promise<PixelsModule> {
  let memory: WebAssembly.Memory | undefined
  const view = () => new DataView(memory!.buffer)
  const wasi = {
    fd_write: (_fd: number, iovs: number, iovsLen: number, written: number) => {
      // Diagnostics from a runtime panic. Counted as written and dropped.
      let n = 0
      for (let i = 0; i < iovsLen; i++) n += view().getUint32(iovs + i * 8 + 4, true)
      view().setUint32(written, n, true)
      return 0
    },
    clock_time_get: (_id: number, _precision: bigint, out: number) => {
      view().setBigUint64(out, BigInt(Date.now()) * 1_000_000n, true)
      return 0
    },
    args_sizes_get: (argc: number, argvBufSize: number) => {
      view().setUint32(argc, 0, true)
      view().setUint32(argvBufSize, 0, true)
      return 0
    },
    args_get: () => 0,
    random_get: (ptr: number, len: number) => {
      crypto.getRandomValues(new Uint8Array(memory!.buffer, ptr, len))
      return 0
    },
  }
  const imports = { wasi_snapshot_preview1: wasi }

  let instance: WebAssembly.Instance
  if (source instanceof ArrayBuffer || ArrayBuffer.isView(source)) {
    instance = (await WebAssembly.instantiate(source, imports)).instance
  } else {
    const res = await source
    if (!res.ok) throw new Error(`loading the RDP decoder: HTTP ${res.status}`)
    try {
      instance = (await WebAssembly.instantiateStreaming(res.clone(), imports)).instance
    } catch {
      // Streaming needs the server to say application/wasm, and whatever
      // serves the console is not ours to configure. The slower path accepts
      // any content type.
      instance = (await WebAssembly.instantiate(await res.arrayBuffer(), imports)).instance
    }
  }
  const px = instance.exports as unknown as PixelsExports
  memory = px.memory
  px._initialize?.()

  return {
    decode(data, width, height, format) {
      const inPtr = px.input(data.byteLength)
      if (data.byteLength > 0) {
        // Read memory.buffer after input(): growing the memory detaches the
        // old one.
        new Uint8Array(px.memory.buffer, inPtr, data.byteLength).set(data)
      }
      const outPtr = px.decode(width, height, format)
      if (outPtr === 0) return null
      return new Uint8ClampedArray(px.memory.buffer.slice(outPtr, outPtr + width * height * 4))
    },
  }
}
