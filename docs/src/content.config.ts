import { defineCollection, z } from 'astro:content'
import { glob } from 'astro/loaders'

// There is deliberately no `status` field: docs/ describes shipped behavior only, so a
// status would have one legal value. Proposals live in specs/, which the site never links.
//
// Nor is there a `section` or `parent` field: both are the file path — `a/b/c.mdx` sits
// under `a/b.mdx`, in section `a`. See src/lib/nav.ts.
const docs = defineCollection({
  loader: glob({ pattern: '**/*.{md,mdx}', base: './src/content/docs' }),
  schema: z.object({
    title: z.string(),
    // Optional because a page whose only job is to name and place a folder has nothing to
    // describe -- it has no body either, and redirects to its first child. See src/lib/nav.ts.
    description: z.string().optional(),
    // Sorts siblings only. Nothing compares an order across two parents.
    order: z.number(),
  }),
})

export const collections = { docs }
