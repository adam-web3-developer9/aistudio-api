import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it } from 'vitest'

import { H3GuideContent } from '..'

afterEach(cleanup)

it('shows the full public H3 workflow and copies the exact generation request', async () => {
  const user = userEvent.setup()
  const router = createRouter({
    routeTree: createRootRoute({ component: H3GuideContent }),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  render(<RouterProvider router={router} />)
  expect(
    await screen.findByRole('heading', { name: 'H3 API documentation' })
  ).toBeVisible()
  expect(screen.getByRole('link', { name: 'API keys' })).toHaveAttribute(
    'href',
    '/keys'
  )
  expect(
    screen.getByRole('heading', { name: '4. Add image or audio references' })
  ).toBeVisible()
  expect(
    screen.getByText(/reservation belongs to a particular GPU allocation/)
  ).toBeVisible()
  expect(
    screen.getByText(/network timeout alone does not establish failure/)
  ).toBeVisible()

  await user.click(screen.getAllByRole('button', { name: 'Copy code' })[0])
  const copied = await navigator.clipboard.readText()
  expect(copied).toContain('https://ai.crypto-compass-web.online')
  expect(copied).toContain('Idempotency-Key: $REQUEST_ID')
  expect(copied).toContain('"model":"h3"')
  expect(copied).toContain('"seconds":"5"')
  expect(copied).toContain('"size":"1344x768"')
})
