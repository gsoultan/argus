/**
 * Picture presets for a remote screen, kept per browser.
 *
 * Size is what the screen opens at: this window's room (the default), or a
 * fixed Full HD or HD, which is lighter on a slow link and scales to fit.
 * Motion is how screen sharing sends what moves: as video (smooth, and
 * light on the network) or as exact pixels (sharp text, more data).
 * Remote Desktop always sends exact pixels.
 *
 * Both are agreed when the screen opens, so a change takes a reconnect.
 */

export type PictureSize = 'fit' | 'fhd' | 'hd'
export interface PicturePrefs {
  size: PictureSize
  motion: 'video' | 'exact'
}

export const PICTURE_DEFAULT: PicturePrefs = { size: 'fit', motion: 'video' }
const KEY = 'argus.desktop.picture'

export const SIZE_LABEL: Record<PictureSize, string> = {
  fit: 'Fit this window',
  fhd: 'Full HD (1920×1080)',
  hd: 'HD (1280×720), for slow links',
}

export function loadPicture(): PicturePrefs {
  try {
    const raw = window.localStorage.getItem(KEY)
    if (!raw) return PICTURE_DEFAULT
    const p = JSON.parse(raw) as Partial<PicturePrefs>
    return {
      size: p.size === 'fhd' || p.size === 'hd' ? p.size : 'fit',
      motion: p.motion === 'exact' ? 'exact' : 'video',
    }
  } catch {
    return PICTURE_DEFAULT
  }
}

export function savePicture(p: PicturePrefs) {
  try {
    window.localStorage.setItem(KEY, JSON.stringify(p))
  } catch {
    // Storage refused: the choice lasts until the tab closes.
  }
}

/** The size to open at: fixed for a preset, else what fits the window. */
export function pictureSize(size: PictureSize, room: { width: number; height: number }): { width: number; height: number } {
  if (size === 'fhd') return { width: 1920, height: 1080 }
  if (size === 'hd') return { width: 1280, height: 720 }
  return room
}
