import { defineCollection, z } from 'astro:content'
import { glob } from 'astro/loaders'

// There is deliberately no `status` field: docs/ describes shipped behavior only, so a
// status would have one legal value. Proposals live in specs/, which the site never links.
const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    description: z.string(),
    section: z.enum(['guides', 'reference']),
    order: z.number(),
  }),
})

export const collections = { docs }
