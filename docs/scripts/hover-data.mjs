// Every hover the language server answers, baked at build time: what a tooltip on this site
// shows is then what an editor shows, by construction. Two outputs — the home page's sample,
// and every `genroc` fence in the docs that is a VALID process.
//
// Validity is the server's own answer, not a guess: a fence it reports an error for is skipped,
// because a hover derived from a definition that does not type is a hover about nothing. Most
// fences are fragments — an `on_error:` list, a task on its own — and they simply get none.
//
// Needs a built genctl (`make build`), the same way the generated reference does. The sweep
// asks column by column and groups the runs that answer alike — a run IS the span the server
// answers over, so nothing here has to parse an expression to find one.
//
// The sample is the bytes the pane renders, so its line and column numbers are the ones the
// page draws with: a comment added to that file would shift every span in here.
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { existsSync, readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const sample = root + 'src/samples/hello.genroc.yaml'
const out = root + 'src/samples/hello.hovers.json'
const content = root + 'src/content/docs'
const fencesOut = root + 'src/samples/fences.hovers.json'

/** The key a fence is found by, from its text alone — the transformer in astro.config.mjs
    hashes what Shiki hands it and looks the spans up here. */
export const fenceKey = (code) => createHash('sha256').update(code.trim()).digest('hex').slice(0, 16)

/** An Astro integration: generates on every `astro build`, and in dev only when the sample has
    moved on — a restart with nothing edited should not wait for a sweep it already has. */
export default function hoverData() {
  return {
    name: 'hover-data',
    hooks: {
      'astro:config:setup': async ({ command, logger, addWatchFile }) => {
        addWatchFile(sample)
        const fresh =
          existsSync(out) && statSync(out).mtimeMs >= statSync(sample).mtimeMs
        if (command === 'build' || !fresh) await generate(logger)
      },
    },
  }
}

async function generate(logger) {
const genctl = [process.env.GENCTL, root + '../genctl', 'genctl'].find(
  (c) => c && (c === 'genctl' || existsSync(c)),
)
if (!genctl) throw new Error('hover-data: no genctl — run `make build` first, or set GENCTL')

const text = readFileSync(sample, 'utf8')
const uri = 'file://' + sample
const srv = spawn(genctl, ['lsp'])
// A build that hangs is worse than one that stops: the server answers in microseconds, so a
// reply that does not come means it died, and the stderr above says how.
srv.stderr.on('data', (d) => logger.error(String(d).trimEnd()))
let killed = false
srv.on('exit', (code, signal) => {
  if (!killed && (code || signal)) logger.error(`genctl lsp exited (${code ?? signal})`)
})
srv.on('error', (e) => {
  logger.error(`could not run ${genctl} (${e.message}) — run \`make build\` first, or set GENCTL`)
  process.exit(1)
})

let buf = Buffer.alloc(0)
const pending = new Map()
const diagnostics = new Map()
srv.stdout.on('data', (d) => {
  buf = Buffer.concat([buf, d])
  for (;;) {
    const head = buf.indexOf('\r\n\r\n')
    if (head < 0) return
    const len = +/Content-Length: (\d+)/.exec(buf.subarray(0, head).toString())[1]
    if (buf.length < head + 4 + len) return
    const msg = JSON.parse(buf.subarray(head + 4, head + 4 + len).toString())
    buf = buf.subarray(head + 4 + len)
    if (msg.method === 'textDocument/publishDiagnostics') {
      diagnostics.set(msg.params.uri, msg.params.diagnostics)
    }
    const settle = pending.get(msg.id)
    if (settle) {
      settle(msg.result)
      pending.delete(msg.id)
    }
  }
})

let id = 0
const send = (method, params, wantsReply = true) =>
  new Promise((resolve, reject) => {
    const msg = { jsonrpc: '2.0', method, params, ...(wantsReply ? { id: ++id } : {}) }
    if (wantsReply) {
      pending.set(id, resolve)
      const stuck = setTimeout(() => reject(new Error(`no reply to ${method} ${JSON.stringify(params.position ?? '')}`)), 10_000)
      const settle = pending.get(id)
      pending.set(id, (r) => {
        clearTimeout(stuck)
        settle(r)
      })
    }
    const body = Buffer.from(JSON.stringify(msg))
    srv.stdin.write('Content-Length: ' + body.length + '\r\n\r\n')
    srv.stdin.write(body)
    if (!wantsReply) resolve()
  })

await send('initialize', { processId: process.pid, rootUri: null, capabilities: {} })
await send('initialized', {}, false)
await send('textDocument/didOpen', { textDocument: { uri, languageId: 'genroc', version: 1, text } }, false)

// A run is trimmed to the token it covers and dropped when that leaves nothing: the server
// answers across the punctuation between two paths, and an underline under a trailing quote
// reads as a typo. Nothing is lost — the token beside it carries the same answer.
async function sweep(uri, text) {
  const hovers = []
  const keep = (run, line) => {
    if (!run) return
    let { from, to } = run
    while (from < to && !/\w/.test(line[from])) from++
    while (to > from && !/\w/.test(line[to - 1])) to--
    if (from < to) hovers.push({ ...run, from, to })
  }
  for (const [i, line] of text.split('\n').entries()) {
    let run = null
    for (let col = 0; col <= line.length; col++) {
      const answer =
        col < line.length
          ? (await send('textDocument/hover', { textDocument: { uri }, position: { line: i, character: col } }))
              ?.contents?.value ?? null
          : null
      if (run && answer === run.markdown) {
        run.to = col + 1
        continue
      }
      keep(run, line)
      run = answer ? { line: i + 1, from: col, to: col + 1, markdown: answer } : null
    }
    keep(run, line)
  }
  return hovers
}

// Every ```genroc fence in the docs, with the file it came from.
function fences() {
  // The content tree carries a node_modules and a dist of its own; walking either is a hang,
  // not a slow build.
  const skip = new Set(['node_modules', 'dist'])
  const walk = (dir) =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
      e.isDirectory()
        ? skip.has(e.name) || e.name.startsWith('.')
          ? []
          : walk(join(dir, e.name))
        : e.name.match(/\.mdx?$/)
          ? [join(dir, e.name)]
          : [],
    )
  return walk(content).flatMap((file) =>
    [...readFileSync(file, 'utf8').matchAll(/^```genroc\s*\n([\s\S]*?)^```/gm)].map((m) => ({
      file: file.slice(content.length + 1),
      code: m[1],
    })),
  )
}

let hovers = []
let found = []
const byKey = {}
let valid = 0
try {
  hovers = await sweep(uri, text)

  // A fence is opened, then asked one question: the server answers in order, so by the time
  // that reply lands, its diagnostics for the document have been published.
  found = fences()
  for (const [i, fence] of found.entries()) {
    const fenceUri = `file://${content}/fence-${i}.genroc.yaml`
    await send(
      'textDocument/didOpen',
      { textDocument: { uri: fenceUri, languageId: 'genroc', version: 1, text: fence.code } },
      false,
    )
    await send('textDocument/hover', { textDocument: { uri: fenceUri }, position: { line: 0, character: 0 } })
    if ((diagnostics.get(fenceUri) ?? []).some((d) => d.severity === 1)) continue
    const spans = await sweep(fenceUri, fence.code)
    if (!spans.length) continue
    byKey[fenceKey(fence.code)] = spans
    valid++
  }
} finally {
  // A child left running holds the build open long after the error that stopped it.
  killed = true
  srv.kill()
}

writeFileSync(out, JSON.stringify(hovers, null, 2) + '\n')
writeFileSync(fencesOut, JSON.stringify(byKey, null, 2) + '\n')
const spans = Object.values(byKey).reduce((n, s) => n + s.length, 0)
logger.info(
  `${hovers.length} spans over the sample; ${spans} over ${valid} of ${found.length} genroc fences ` +
    `(the rest are fragments the server cannot type)`,
)
}
