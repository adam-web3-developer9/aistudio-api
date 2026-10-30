import { createFileRoute } from '@tanstack/react-router'

import { H3Documentation } from '@/features/h3-documentation'

export const Route = createFileRoute('/docs/h3')({
  component: H3Documentation,
})
