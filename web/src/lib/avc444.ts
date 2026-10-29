/**
 * AVC444v2: a full-colour picture from two 4:2:0 H.264 streams
 * (MS-RDPEGFX 3.3.8.3.3).
 *
 * H.264 as browsers decode it keeps one colour sample for every four pixels,
 * which smears coloured text. Windows sends the rest of the colour as a second
 * stream: the main view is an ordinary 4:2:0 picture, and the auxiliary view
 * carries the chroma samples the main one dropped, packed into its luma and
 * chroma planes. Put back together they give every pixel its own colour.
 *
 * Where each full-resolution chroma sample comes from, for pixel (x, y):
 *
 *   x odd                 the auxiliary luma plane: U in its left half at
 *                         (x/2, y), V in its right half
 *   x even, y odd         the auxiliary chroma planes: for x % 4 == 0 its U
 *                         plane, for x % 4 == 2 its V plane; U in the left
 *                         half at (x/4, y/2), V in the right half
 *   x even, y even        the main view's chroma, which is the average of the
 *                         2x2 block; the block's other three samples are known,
 *                         so the fourth is 4 * average - the others. That is
 *                         kept only when it differs from the average by 30 or
 *                         more, as Windows' own client does, because otherwise
 *                         rounding in the encoder shows as noise.
 *
 * The same reconstruction runs here on the CPU (combineV2) and on the GPU
 * (Avc444Gl, the console's path); the CPU one is the reference the GPU one is
 * tested against.
 */

/** One picture's planes: Y at full size, U and V at half size each way. */
export interface Planes {
  width: number
  height: number
  y: Uint8Array
  u: Uint8Array
  v: Uint8Array
  /** Bytes per row of y; u and v rows are chromaStride. */
  yStride: number
  chromaStride: number
  /** How the stream says its colour is encoded. */
  colour: Colour
  /** Where the decoder's copy lands before it is split into planes. */
  raw?: Uint8Array
}

/**
 * How YUV becomes RGB: the stream's matrix and range, as its parameter sets
 * say. A stream that says nothing gets BT.601 at full range, which is what
 * Windows' own client assumes.
 */
export interface Colour {
  bt709: boolean
  fullRange: boolean
}

export const DEFAULT_COLOUR: Colour = { bt709: false, fullRange: true }

/** The colour a decoded picture says it has. */
export function frameColour(cs: VideoColorSpace | null | undefined): Colour {
  return {
    bt709: cs?.matrix === 'bt709',
    fullRange: cs?.fullRange ?? DEFAULT_COLOUR.fullRange,
  }
}

/**
 * The conversion as numbers: luma's offset and scale, chroma's scale, then
 * red from V, green from U and V, blue from U.
 */
export function colourMatrix(c: Colour): number[] {
  const [kr, kb] = c.bt709 ? [0.2126, 0.0722] : [0.299, 0.114]
  const kg = 1 - kr - kb
  const [offset, yScale, cScale] = c.fullRange ? [0, 1, 1] : [16, 255 / 219, 255 / 224]
  return [
    offset,
    yScale,
    cScale,
    2 * (1 - kr),
    (2 * kb * (1 - kb)) / kg,
    (2 * kr * (1 - kr)) / kg,
    2 * (1 - kb),
  ]
}

export function emptyPlanes(width: number, height: number): Planes {
  const cw = (width + 1) >> 1
  const ch = (height + 1) >> 1
  return {
    width,
    height,
    y: new Uint8Array(width * height),
    u: new Uint8Array(cw * ch),
    v: new Uint8Array(cw * ch),
    yStride: width,
    chromaStride: cw,
    colour: DEFAULT_COLOUR,
  }
}

/** The colour of one pixel, as full-resolution U and V, from the two views. */
export function chromaV2(main: Planes, aux: Planes, x: number, y: number): [number, number] {
  const half = (aux.width + 1) >> 1
  const quarter = (aux.width + 3) >> 2
  const auxY = (px: number, py: number) => aux.y[py * aux.yStride + px] ?? 0
  const at = (px: number, py: number): [number, number] => {
    if (px & 1) {
      const i = px >> 1
      return [auxY(i, py), auxY(half + i, py)]
    }
    if (py & 1) {
      const plane = px & 2 ? aux.v : aux.u
      const row = (py >> 1) * aux.chromaStride
      const i = px >> 2
      return [plane[row + i] ?? 0, plane[row + quarter + i] ?? 0]
    }
    const i = (py >> 1) * main.chromaStride + (px >> 1)
    return [main.u[i] ?? 0, main.v[i] ?? 0]
  }
  if (x & 1 || y & 1) return at(x, y)

  // The top-left of a 2x2 block: what the average and the other three imply.
  const [u0, v0] = at(x, y)
  if (x + 1 >= main.width || y + 1 >= main.height) return [u0, v0]
  const [u1, v1] = at(x + 1, y)
  const [u2, v2] = at(x, y + 1)
  const [u3, v3] = at(x + 1, y + 1)
  return [settle(4 * u0 - u1 - u2 - u3, u0), settle(4 * v0 - v1 - v2 - v3, v0)]
}

/** The reconstructed sample, unless it is within rounding of the average. */
function settle(value: number, average: number): number {
  const v = Math.min(255, Math.max(0, value))
  return Math.abs(v - average) < 30 ? average : v
}

/** One pixel to red, green and blue. */
export function rgb(y: number, u: number, v: number, colour: Colour = DEFAULT_COLOUR): [number, number, number] {
  const [offset, yScale, cScale, rv, gu, gv, bu] = colourMatrix(colour) as [
    number, number, number, number, number, number, number,
  ]
  const l = (y - offset) * yScale
  const d = (u - 128) * cScale
  const e = (v - 128) * cScale
  const clamp = (n: number) => Math.min(255, Math.max(0, Math.round(n)))
  return [clamp(l + rv * e), clamp(l - gu * d - gv * e), clamp(l + bu * d)]
}

/** The whole picture as RGBA, on the CPU. */
export function combineV2(main: Planes, aux: Planes): Uint8ClampedArray {
  const out = new Uint8ClampedArray(main.width * main.height * 4)
  for (let y = 0; y < main.height; y++) {
    for (let x = 0; x < main.width; x++) {
      const [u, v] = chromaV2(main, aux, x, y)
      const [r, g, b] = rgb(main.y[y * main.yStride + x] ?? 0, u, v, main.colour)
      const o = (y * main.width + x) * 4
      out[o] = r
      out[o + 1] = g
      out[o + 2] = b
      out[o + 3] = 255
    }
  }
  return out
}

/**
 * The two views Windows would send for a picture given as full-resolution
 * Y, U and V planes: the inverse of chromaV2, for tests.
 */
export function splitV2(
  width: number,
  height: number,
  y444: Uint8Array,
  u444: Uint8Array,
  v444: Uint8Array,
): { main: Planes; aux: Planes } {
  const main = emptyPlanes(width, height)
  const aux = emptyPlanes(width, height)
  main.y.set(y444)
  const half = (width + 1) >> 1
  const quarter = (width + 3) >> 2
  const u = (x: number, y: number) => u444[y * width + x] ?? 0
  const v = (x: number, y: number) => v444[y * width + x] ?? 0
  for (let y = 0; y < height; y += 2) {
    for (let x = 0; x < width; x += 2) {
      const block = (f: (x: number, y: number) => number) => {
        let sum = 0
        let n = 0
        for (const [dx, dy] of [[0, 0], [1, 0], [0, 1], [1, 1]] as const) {
          if (x + dx < width && y + dy < height) {
            sum += f(x + dx, y + dy)
            n++
          }
        }
        return Math.round(sum / n)
      }
      const i = (y >> 1) * main.chromaStride + (x >> 1)
      main.u[i] = block(u)
      main.v[i] = block(v)
    }
  }
  for (let y = 0; y < height; y++) {
    for (let x = 1; x < width; x += 2) {
      aux.y[y * aux.yStride + (x >> 1)] = u(x, y)
      aux.y[y * aux.yStride + half + (x >> 1)] = v(x, y)
    }
  }
  for (let y = 1; y < height; y += 2) {
    const row = (y >> 1) * aux.chromaStride
    for (let x = 0; x < width; x += 2) {
      const plane = x & 2 ? aux.v : aux.u
      plane[row + (x >> 2)] = u(x, y)
      plane[row + quarter + (x >> 2)] = v(x, y)
    }
  }
  return { main, aux }
}

/**
 * Copies a decoded picture's planes out of a VideoFrame. Decoders give I420
 * or, in hardware, NV12, whose interleaved chroma is split here.
 */
export async function framePlanes(frame: VideoFrame, into?: Planes): Promise<Planes | null> {
  const width = frame.displayWidth
  const height = frame.displayHeight
  const format = frame.format
  if (format !== 'I420' && format !== 'NV12' && format !== 'I420A') return null
  const rect = { x: 0, y: 0, width, height }
  const size = frame.allocationSize({ rect })
  const p = into && into.width === width && into.height === height ? into : emptyPlanes(width, height)
  // Kept with the planes and reused: two streams at thirty pictures a second
  // would otherwise be sixty fresh buffers a second of megabytes each. So one
  // copy into a set of planes at a time: the caller waits for the last.
  if (!p.raw || p.raw.byteLength < size) p.raw = new Uint8Array(size)
  const buf = p.raw
  const layout = await frame.copyTo(buf, { rect })
  p.colour = frameColour(frame.colorSpace)
  const [yl, ul, vl] = layout
  if (!yl || !ul) return null
  for (let row = 0; row < height; row++) {
    p.y.set(buf.subarray(yl.offset + row * yl.stride, yl.offset + row * yl.stride + width), row * p.yStride)
  }
  const cw = p.chromaStride
  const ch = (height + 1) >> 1
  if (format === 'NV12') {
    for (let row = 0; row < ch; row++) {
      const from = ul.offset + row * ul.stride
      for (let i = 0; i < cw; i++) {
        p.u[row * cw + i] = buf[from + 2 * i] ?? 0
        p.v[row * cw + i] = buf[from + 2 * i + 1] ?? 0
      }
    }
  } else {
    if (!vl) return null
    for (let row = 0; row < ch; row++) {
      p.u.set(buf.subarray(ul.offset + row * ul.stride, ul.offset + row * ul.stride + cw), row * cw)
      p.v.set(buf.subarray(vl.offset + row * vl.stride, vl.offset + row * vl.stride + cw), row * cw)
    }
  }
  return p
}

const VERTEX = `#version 300 es
in vec2 corner;
void main() { gl_Position = vec4(corner, 0.0, 1.0); }
`

/** chromaV2 and rgb, per pixel. Planes are R8 textures read with texelFetch. */
const FRAGMENT = `#version 300 es
precision highp float;
precision highp int;
uniform highp usampler2D mainY, mainU, mainV, auxY, auxU, auxV;
uniform ivec2 size;
// Without the second view yet: the main view's colour, 4:2:0, as it is.
uniform bool hasAux;
// colourMatrix: luma offset and scale, chroma scale, then rv, gu, gv, bu.
uniform float yOffset, yScale, cScale, rv, gu, gv, bu;
out vec4 colour;

float sample1(highp usampler2D t, int x, int y) { return float(texelFetch(t, ivec2(x, y), 0).r); }

vec2 at(int x, int y) {
  if (!hasAux) return vec2(sample1(mainU, x / 2, y / 2), sample1(mainV, x / 2, y / 2));
  int halfW = (size.x + 1) / 2;
  int quarter = (size.x + 3) / 4;
  if ((x & 1) == 1) {
    int i = x / 2;
    return vec2(sample1(auxY, i, y), sample1(auxY, halfW + i, y));
  }
  if ((y & 1) == 1) {
    int i = x / 4;
    int row = y / 2;
    if ((x & 2) == 2) return vec2(sample1(auxV, i, row), sample1(auxV, quarter + i, row));
    return vec2(sample1(auxU, i, row), sample1(auxU, quarter + i, row));
  }
  return vec2(sample1(mainU, x / 2, y / 2), sample1(mainV, x / 2, y / 2));
}

float settle(float value, float average) {
  float v = clamp(value, 0.0, 255.0);
  return abs(v - average) < 30.0 ? average : v;
}

void main() {
  // gl_FragCoord counts rows from the bottom; pictures from the top.
  int x = int(gl_FragCoord.x);
  int y = size.y - 1 - int(gl_FragCoord.y);
  vec2 uv = at(x, y);
  if (hasAux && (x & 1) == 0 && (y & 1) == 0 && x + 1 < size.x && y + 1 < size.y) {
    vec2 others = at(x + 1, y) + at(x, y + 1) + at(x + 1, y + 1);
    vec2 recon = 4.0 * uv - others;
    uv = vec2(settle(recon.x, uv.x), settle(recon.y, uv.y));
  }
  float luma = (sample1(mainY, x, y) - yOffset) * yScale;
  float d = (uv.x - 128.0) * cScale;
  float e = (uv.y - 128.0) * cScale;
  vec3 c = vec3(luma + rv * e, luma - gu * d - gv * e, luma + bu * d);
  colour = vec4(clamp(floor(c + 0.5), 0.0, 255.0) / 255.0, 1.0);
}
`

/**
 * The reconstruction on the GPU: the six planes go up as textures, one draw
 * puts the picture in the canvas, and the caller copies what changed from
 * there onto its surface.
 */
export class Avc444Gl {
  readonly canvas: OffscreenCanvas
  private gl: WebGL2RenderingContext
  private textures: WebGLTexture[] = []
  private sizeAt: WebGLUniformLocation | null
  private hasAuxAt: WebGLUniformLocation | null
  private program: WebGLProgram

  private constructor(canvas: OffscreenCanvas, gl: WebGL2RenderingContext, program: WebGLProgram) {
    this.canvas = canvas
    this.gl = gl
    this.program = program
    gl.useProgram(program)
    const buf = gl.createBuffer()
    gl.bindBuffer(gl.ARRAY_BUFFER, buf)
    gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 1, -1, -1, 1, 1, 1]), gl.STATIC_DRAW)
    const corner = gl.getAttribLocation(program, 'corner')
    gl.enableVertexAttribArray(corner)
    gl.vertexAttribPointer(corner, 2, gl.FLOAT, false, 0, 0)
    ;['mainY', 'mainU', 'mainV', 'auxY', 'auxU', 'auxV'].forEach((name, i) => {
      const t = gl.createTexture()
      if (!t) throw new Error('no texture')
      gl.activeTexture(gl.TEXTURE0 + i)
      gl.bindTexture(gl.TEXTURE_2D, t)
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.NEAREST)
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.NEAREST)
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
      gl.texParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)
      // A pixel to start with, so no sampler is ever without an image.
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.R8UI, 1, 1, 0, gl.RED_INTEGER, gl.UNSIGNED_BYTE, new Uint8Array(1))
      gl.uniform1i(gl.getUniformLocation(program, name), i)
      this.textures.push(t)
    })
    gl.pixelStorei(gl.UNPACK_ALIGNMENT, 1)
    this.sizeAt = gl.getUniformLocation(program, 'size')
    this.hasAuxAt = gl.getUniformLocation(program, 'hasAux')
  }

  /** A combiner, or null where this browser has no WebGL 2 in a worker. */
  static create(): Avc444Gl | null {
    if (typeof OffscreenCanvas === 'undefined') return null
    const canvas = new OffscreenCanvas(1, 1)
    const gl = canvas.getContext('webgl2', { antialias: false, premultipliedAlpha: false })
    if (!gl) return null
    const shader = (type: number, src: string) => {
      const s = gl.createShader(type)
      if (!s) return null
      gl.shaderSource(s, src)
      gl.compileShader(s)
      return gl.getShaderParameter(s, gl.COMPILE_STATUS) ? s : null
    }
    const vs = shader(gl.VERTEX_SHADER, VERTEX)
    const fs = shader(gl.FRAGMENT_SHADER, FRAGMENT)
    const program = gl.createProgram()
    if (!vs || !fs || !program) return null
    gl.attachShader(program, vs)
    gl.attachShader(program, fs)
    gl.linkProgram(program)
    if (!gl.getProgramParameter(program, gl.LINK_STATUS)) return null
    return new Avc444Gl(canvas, gl, program)
  }

  /**
   * Draws the whole picture: in full colour, or without the second view, as
   * the main one's 4:2:0. The caller closes the bitmap.
   */
  draw(main: Planes, aux: Planes | null): ImageBitmap {
    this.render(main, aux)
    return this.canvas.transferToImageBitmap()
  }

  private render(main: Planes, aux: Planes | null) {
    const gl = this.gl
    const { width, height } = main
    if (this.canvas.width !== width || this.canvas.height !== height) {
      this.canvas.width = width
      this.canvas.height = height
    }
    gl.viewport(0, 0, width, height)
    const cw = main.chromaStride
    const ch = (height + 1) >> 1
    const planes: Array<[Uint8Array, number, number]> = [
      [main.y, main.yStride, height],
      [main.u, cw, ch],
      [main.v, cw, ch],
    ]
    if (aux) {
      planes.push([aux.y, aux.yStride, height], [aux.u, aux.chromaStride, ch], [aux.v, aux.chromaStride, ch])
    }
    planes.forEach(([data, w, h], i) => {
      gl.activeTexture(gl.TEXTURE0 + i)
      gl.bindTexture(gl.TEXTURE_2D, this.textures[i] ?? null)
      gl.texImage2D(gl.TEXTURE_2D, 0, gl.R8UI, w, h, 0, gl.RED_INTEGER, gl.UNSIGNED_BYTE, data)
    })
    gl.uniform2i(this.sizeAt, width, height)
    gl.uniform1i(this.hasAuxAt, aux ? 1 : 0)
    const m = colourMatrix(main.colour)
    ;['yOffset', 'yScale', 'cScale', 'rv', 'gu', 'gv', 'bu'].forEach((name, i) => {
      gl.uniform1f(gl.getUniformLocation(this.program, name), m[i] ?? 0)
    })
    gl.drawArrays(gl.TRIANGLE_STRIP, 0, 4)
  }

  /** The picture's pixels, top row first, for tests. */
  read(main: Planes, aux: Planes | null): Uint8Array {
    this.render(main, aux)
    const gl = this.gl
    const { width, height } = this.canvas
    const raw = new Uint8Array(width * height * 4)
    gl.readPixels(0, 0, width, height, gl.RGBA, gl.UNSIGNED_BYTE, raw)
    const out = new Uint8Array(raw.length)
    for (let row = 0; row < height; row++) {
      out.set(raw.subarray((height - 1 - row) * width * 4, (height - row) * width * 4), row * width * 4)
    }
    return out
  }
}
