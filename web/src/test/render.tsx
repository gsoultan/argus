import { render as rtlRender } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { theme } from '~/theme'

/**
 * Renders a component inside the providers it actually runs under.
 *
 * No router: components that need one are exercised through their routes, and
 * TanStack Router resolves its initial match asynchronously, so a synchronous
 * render against it yields an empty document and a confusing failure.
 */
export function renderWithProviders(node: React.ReactNode) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  return rtlRender(
    <MantineProvider theme={theme} defaultColorScheme="dark" forceColorScheme="dark">
      <QueryClientProvider client={queryClient}>{node}</QueryClientProvider>
    </MantineProvider>,
  )
}
