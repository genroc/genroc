// Every hover the language server answers for the home page's sample, baked at build time:
// what the editor pane shows in a tooltip is then what an editor shows, by construction.
//
// Needs a built genctl (`make build`), the same way the generated reference does. The sweep
// asks column by column and groups the runs that answer alike — a run IS the span the server
// answers over, so nothing here has to parse an expression to find one.
//
// The sample is the bytes the pane renders, so its line and column numbers are the ones the
// page draws with: a comment added to that file would shift every span in here.
import { spawn } from 'node:child_process'
import { existsSync, readFileSync, statSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const root = fileURLToPath(new URL('..', import.meta.url))
const sample = root + 'src/samples/hello.genroc.yaml'
const out = root + 'src/samples/hello.hovers.json'

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
srv.on('error', (e) => {
  logger.error(`could not run ${genctl} (${e.message}) — run \`make build\` first, or set GENCTL`)
  process.exit(1)
})

let buf = Buffer.alloc(0)
const pending = new Map()
srv.stdout.on('data', (d) => {
  buf = Buffer.concat([buf, d])
  for (;;) {
    const head = buf.indexOf('\r\n\r\n')
    if (head < 0) return
    const len = +/Content-Length: (\d+)/.exec(buf.subarray(0, head).toString())[1]
    if (buf.length < head + 4 + len) return
    const msg = JSON.parse(buf.subarray(head + 4, head + 4 + len).toString())
    buf = buf.subarray(head + 4 + len)
    const settle = pending.get(msg.id)
    if (settle) {
      settle(msg.result)
      pending.delete(msg.id)
    }
  }
})

let id = 0
const send = (method, params, wantsReply = true) =>
  new Promise((resolve) => {
    const msg = { jsonrpc: '2.0', method, params, ...(wantsReply ? { id: ++id } : {}) }
    if (wantsReply) pending.set(id, resolve)
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
srv.kill()

writeFileSync(out, JSON.stringify(hovers, null, 2) + '\n')
logger.info(`${hovers.length} spans over ${text.split('\n').length} lines of ${sample.slice(root.length)}`)
}
