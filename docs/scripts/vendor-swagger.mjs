// Copies Swagger UI out of node_modules into public/, because the alternative is a CDN tag and
// this site is built once per tag and never rebuilt (specs/docs-site.md): a pinned `@5` would
// silently re-render an archived /v1/ under whatever 5.x ships years later, or stop resolving.
// Vendored, the archive keeps the bytes it was published with.
import { copyFileSync, mkdirSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'

const require = createRequire(import.meta.url)
const from = dirname(require.resolve('swagger-ui-dist/swagger-ui.css'))
const into = new URL('../public/swagger/', import.meta.url).pathname

mkdirSync(into, { recursive: true })
for (const file of ['swagger-ui.css', 'swagger-ui-bundle.js', 'swagger-ui-standalone-preset.js']) {
  copyFileSync(join(from, file), join(into, file))
}
console.log(`vendored swagger-ui ${require('swagger-ui-dist/package.json').version} into public/swagger/`)
