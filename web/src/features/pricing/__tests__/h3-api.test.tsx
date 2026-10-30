/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRouter,
  RouterContextProvider,
} from '@tanstack/react-router'
import { act, cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, expect, it } from 'vitest'

import zh from '@/i18n/locales/zh.json'

import { ModelDetailsApi } from '../components/model-details-api'
import { buildSupportedParameters } from '../lib/mock-stats'
import type { PricingModel } from '../types'

const model: PricingModel = {
  id: 64,
  model_name: 'h3',
  quota_type: 0,
  model_ratio: 0,
  completion_ratio: 0,
  enable_groups: ['default'],
  supported_endpoint_types: ['openai-video'],
}

afterEach(async () => {
  cleanup()
  i18next.removeResourceBundle('zhCN', 'translation')
  await i18next.changeLanguage('en')
})

it('shows valid H3 submission and retrieval examples in every offered language', async () => {
  const user = userEvent.setup()
  const client = new QueryClient({
    defaultOptions: { queries: { enabled: false } },
  })
  const router = createRouter({
    routeTree: createRootRoute(),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  const { container } = render(
    <RouterContextProvider router={router}>
      <QueryClientProvider client={client}>
        <ModelDetailsApi
          model={model}
          endpointMap={{
            'openai-video': { path: '/v1/videos', method: 'POST' },
          }}
        />
      </QueryClientProvider>
    </RouterContextProvider>
  )
  expect(
    screen.getByRole('link', { name: 'H3 API documentation' })
  ).toHaveAttribute('href', '/docs/h3')
  for (const language of ['cURL', 'Python', 'TypeScript', 'JavaScript']) {
    await user.click(screen.getByRole('tab', { name: language }))
    const example = container.querySelector('.cm-content')?.textContent ?? ''
    expect(example).toContain('/v1/videos')
    expect(example).toContain('Idempotency-Key')
    expect(example).toContain('1344x768')
    expect(example).toContain('seconds')
    expect(example).not.toContain('messages')
    expect(example).not.toContain('chat.completions')
  }
  expect(screen.getByText(/\$0\.09.*\$0\.45/)).toBeVisible()
  i18next.addResourceBundle('zhCN', 'translation', zh.translation)
  await act(() => i18next.changeLanguage('zhCN'))
  expect(screen.getByText(/按请求视频时长.*\$0\.09.*\$0\.45/)).toBeVisible()
  expect(screen.getByText(/GET \/v1\/videos\/.*status/)).toBeVisible()
})

it('documents H3 duration and size constraints without unsupported chat or FPS controls', () => {
  const parameters = buildSupportedParameters(model)
  expect(
    parameters.find((parameter) => parameter.name === 'seconds')
  ).toMatchObject({
    defaultValue: 5,
    range: '4 ~ 15',
  })
  expect(
    parameters.find((parameter) => parameter.name === 'size')
  ).toMatchObject({
    defaultValue: '1344x768',
  })
  expect(parameters.map((parameter) => parameter.name)).not.toContain(
    'temperature'
  )
  expect(parameters.map((parameter) => parameter.name)).not.toContain('fps')
  expect(parameters.map((parameter) => parameter.name)).toContain(
    'metadata.direct_media'
  )
})
