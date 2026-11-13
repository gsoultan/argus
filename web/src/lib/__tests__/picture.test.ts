import { afterEach, describe, expect, it } from 'vitest'
import { PICTURE_DEFAULT, loadPicture, pictureSize, savePicture, slowLink } from '~/lib/picture'

/** Picture presets for remote screens. */

afterEach(() => window.localStorage.clear())

describe('picture presets', () => {
  it('fits the window unless a fixed size is chosen', () => {
    const room = { width: 1504, height: 820 }
    expect(pictureSize('fit', room)).toEqual(room)
    expect(pictureSize('fhd', room)).toEqual({ width: 1920, height: 1080 })
    expect(pictureSize('hd', room)).toEqual({ width: 1280, height: 720 })
  })

  it('remembers the choice, and ignores what it does not know', () => {
    expect(loadPicture()).toEqual(PICTURE_DEFAULT)
    savePicture({ size: 'hd', motion: 'exact' })
    expect(loadPicture()).toEqual({ size: 'hd', motion: 'exact' })
    window.localStorage.setItem('argus.desktop.picture', '{"size":"8k","motion":"?"}')
    expect(loadPicture()).toEqual(PICTURE_DEFAULT)
    window.localStorage.setItem('argus.desktop.picture', 'not json')
    expect(loadPicture()).toEqual(PICTURE_DEFAULT)
  })

  it('opens as for a slow link when the size or the browser says so', () => {
    expect(slowLink('fit', undefined)).toBe(false)
    expect(slowLink('hd', undefined)).toBe(true)
    expect(slowLink('fit', { effectiveType: '4g', downlink: 10 })).toBe(false)
    expect(slowLink('fit', { effectiveType: '3g', downlink: 10 })).toBe(true)
    expect(slowLink('fit', { effectiveType: '4g', downlink: 1.4 })).toBe(true)
    expect(slowLink('fit', { saveData: true })).toBe(true)
    // A browser that cannot tell reports nothing, or zero.
    expect(slowLink('fit', { downlink: 0 })).toBe(false)
  })
})
