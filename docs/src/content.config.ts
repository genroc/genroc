import { defineCollection, z } from 'astro:content'
import { glob } from 'astro/loaders'

// No `status` field: docs/ describes shipped behaviour only (proposals live in specs/). No
// `section` or `parent` either: both are the file path (src/lib/nav.ts).
const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    // Optional because a page whose only job is to name and place a folder has nothing to
    // describe -- it has no body either, and redirects to its first child. See src/lib/nav.ts.
    description: z.string().optional(),
    // Sorts siblings only. Nothing compares an order across two parents.
    order: z.number(),
    // Whether this page's descendants collapse when the reader is elsewhere. Inherited by
    // every level below unless one sets it again; unset anywhere above means never collapse.
    autoCollapse: z.boolean().optional(),
  }),
})

export const collections = { docs }
