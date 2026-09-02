import { forwardRef } from 'react'
import { Button, type ButtonProps } from '@mantine/core'
import { createLink, type LinkComponent } from '@tanstack/react-router'

/**
 * Mantine `Button` wired through TanStack Router's `createLink`.
 *
 * `component={Link}` works at runtime but collapses the router's generic
 * inference, so `to` stops narrowing and `params` degrades to a reducer
 * signature. `createLink` restores both — but it cannot see through Mantine's
 * polymorphic generic, so we pin the props to a concrete forwardRef wrapper
 * first. That gives real anchor semantics: middle-click, cmd-click and
 * copy-link all behave, and a typo in a route path is a compile error.
 */

interface ButtonAnchorProps extends Omit<ButtonProps, 'component'> {
  'aria-label'?: string
}

const ButtonAnchor = forwardRef<HTMLAnchorElement, ButtonAnchorProps>((props, ref) => (
  <Button component="a" ref={ref} {...props} />
))
ButtonAnchor.displayName = 'ButtonAnchor'

const CreatedButtonLink = createLink(ButtonAnchor)

export const ButtonLink: LinkComponent<typeof ButtonAnchor> = (props) => (
  <CreatedButtonLink preload="intent" {...props} />
)
