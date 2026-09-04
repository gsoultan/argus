import { describe, expect, it, vi } from 'vitest'
import { render, screen, fireEvent } from '@testing-library/react'
import { PlayerControls } from '~/components/PlayerControls'
import { ErrorState } from '~/components/ErrorState'
import { rowNav } from '~/components/primitives'
import { renderWithProviders as ui } from '~/test/render'

const base = {
  time: 30,
  total: 120,
  playing: false,
  speed: '1',
  onPlayPause: () => {},
  onSeek: () => {},
  onSpeed: () => {},
}

describe('PlayerControls', () => {
  it('labels every icon-only control for assistive tech', () => {
    ui(<PlayerControls {...base} />)
    expect(screen.getByLabelText('Play')).toBeInTheDocument()
    expect(screen.getByLabelText('Back 10 seconds')).toBeInTheDocument()
    expect(screen.getByLabelText('Forward 10 seconds')).toBeInTheDocument()
    // Role-scoped: Mantine renders a combobox input plus a hidden native
    // select for form submission, and both carry the label.
    expect(screen.getByRole('combobox', { name: 'Playback speed' })).toBeInTheDocument()
  })

  it('shows the transport position', () => {
    ui(<PlayerControls {...base} />)
    expect(screen.getByText('00:30 / 02:00')).toBeInTheDocument()
  })

  it('switches the label when playing, so the button says what it will do', () => {
    ui(<PlayerControls {...base} playing />)
    expect(screen.getByLabelText('Pause')).toBeInTheDocument()
  })

  it('skips by ten seconds from the current position', () => {
    const onSeek = vi.fn()
    ui(<PlayerControls {...base} onSeek={onSeek} />)
    fireEvent.click(screen.getByLabelText('Back 10 seconds'))
    expect(onSeek).toHaveBeenCalledWith(20)
    fireEvent.click(screen.getByLabelText('Forward 10 seconds'))
    expect(onSeek).toHaveBeenCalledWith(40)
  })

  describe('keyboard shortcuts', () => {
    it('plays, pauses and seeks from anywhere on the page', () => {
      const onPlayPause = vi.fn()
      const onSeek = vi.fn()
      ui(<PlayerControls {...base} onPlayPause={onPlayPause} onSeek={onSeek} />)

      fireEvent.keyDown(window, { code: 'Space' })
      expect(onPlayPause).toHaveBeenCalled()

      fireEvent.keyDown(window, { code: 'ArrowLeft' })
      expect(onSeek).toHaveBeenLastCalledWith(25)
      fireEvent.keyDown(window, { code: 'ArrowRight', shiftKey: true })
      expect(onSeek).toHaveBeenLastCalledWith(60)
    })

    /**
     * A termination reason contains spaces. If the player swallowed them the
     * operator would be typing into a form while the recording started playing.
     */
    it('ignores keys while the user is typing', () => {
      const onPlayPause = vi.fn()
      ui(
        <>
          <textarea data-testid="reason" />
          <PlayerControls {...base} onPlayPause={onPlayPause} />
        </>,
      )
      fireEvent.keyDown(screen.getByTestId('reason'), { code: 'Space' })
      expect(onPlayPause).not.toHaveBeenCalled()
    })
  })
})

describe('ErrorState', () => {
  it('shows the underlying message rather than hiding it', () => {
    ui(<ErrorState error={new Error('recording chunk 12 failed to parse')} />)
    expect(screen.getByText(/chunk 12 failed to parse/)).toBeInTheDocument()
  })

  it('runs the retry handler when one is given', () => {
    const reset = vi.fn()
    ui(<ErrorState error="boom" reset={reset} />)
    fireEvent.click(screen.getByRole('button', { name: /try again/i }))
    expect(reset).toHaveBeenCalled()
  })

  it('omits the retry button when recovery is not possible', () => {
    ui(<ErrorState error="boom" />)
    expect(screen.queryByRole('button', { name: /try again/i })).not.toBeInTheDocument()
  })
})

describe('rowNav', () => {
  it('makes a row reachable and operable by keyboard', () => {
    const go = vi.fn()
    const props = rowNav(go)
    expect(props.tabIndex).toBe(0)
    expect(props.role).toBe('link')

    render(<tr {...props} data-testid="row" />, {
      container: document.body.appendChild(document.createElement('tbody')),
    })
    const row = screen.getByTestId('row')

    fireEvent.click(row)
    expect(go).toHaveBeenCalledTimes(1)
    fireEvent.keyDown(row, { key: 'Enter' })
    expect(go).toHaveBeenCalledTimes(2)
    fireEvent.keyDown(row, { key: ' ' })
    expect(go).toHaveBeenCalledTimes(3)
    fireEvent.keyDown(row, { key: 'a' })
    expect(go).toHaveBeenCalledTimes(3)
  })
})
