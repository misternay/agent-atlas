import { execFileSync, spawn } from 'node:child_process'
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises'
import { createServer, createConnection } from 'node:net'
import { join, resolve } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'

const binaries = resolve(process.argv[2] ?? 'bin')
const dir = await mkdtemp('/tmp/agent-atlas-browser-')
const home = join(dir, 'data')
const profile = join(dir, 'chrome')
const chromePath = process.env.CHROME_BIN ?? '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'
await mkdir(home, { mode: 0o700 })
const listener = createServer()
await new Promise((resolve, reject) => listener.listen(0, '127.0.0.1', resolve).once('error', reject))
const port = listener.address().port
await new Promise((resolve) => listener.close(resolve))
const url = 'http://127.0.0.1:' + port
await writeFile(join(home, 'config.json'), JSON.stringify({ addr: '127.0.0.1:' + port }), { mode: 0o600 })
const atlas = spawn(join(binaries, 'atlasd'), ['--home', home, '--log', join(home, 'atlasd.log')], { stdio: 'ignore' })
let chrome, cdp
async function until(fn, timeout=10000) {
  const deadline = Date.now() + timeout
  let lastError
  do {
    try {
      const value = await fn()
      if (value) return value
    } catch (e) {
      lastError = e
    }
    await delay(20)
  } while (Date.now() < deadline)
  throw new Error('timeout: ' + String(lastError))
}
try {
  await until(async () => (await fetch(url + '/api/v1/health')).ok)
  console.log('atlasd idle RSS KiB:', execFileSync('ps', ['-o', 'rss=', '-p', String(atlas.pid)], { encoding: 'utf8' }).trim())
  const signIn = execFileSync(join(binaries, 'atlas'), ['open', '--print'], {
    encoding: 'utf8', env: { ...process.env, ATLAS_HOME: home },
  }).trim()
  chrome = spawn(chromePath, [
    '--headless=new', '--no-first-run', '--no-default-browser-check', '--disable-background-networking',
    '--disable-sync', '--disable-component-update', '--disable-extensions', '--window-size=1600,1000',
    '--remote-debugging-port=0', '--user-data-dir=' + profile, 'about:blank',
  ], { stdio: 'ignore' })
  let chromeError
  chrome.once('error', (error) => { chromeError = error })
  const active = await until(async () => {
    if (chromeError) throw chromeError
    return (await readFile(join(profile, 'DevToolsActivePort'), 'utf8')).trim()
  })
  const devToolsUrl = 'http://127.0.0.1:' + active.split('\n')[0]
  const version = await (await fetch(devToolsUrl + '/json/version')).json()
  const [target] = (await (await fetch(devToolsUrl + '/json/list')).json()).filter((t) => t.type === 'page')
  cdp = new WebSocket(target.webSocketDebuggerUrl)
  await new Promise((resolve, reject) => {
    cdp.addEventListener('open', resolve, { once: true })
    cdp.addEventListener('error', reject, { once: true })
  })
  let requestId = 0
  const requests = new Map()
  cdp.addEventListener('message', ({ data }) => {
    const msg = JSON.parse(data)
    const request = requests.get(msg.id)
    if (!request) return
    requests.delete(msg.id)
    if (msg.error) request.reject(new Error(JSON.stringify(msg.error)))
    else request.resolve(msg.result)
  })
  const send = (method, params) => new Promise((resolve, reject) => {
    const id = ++requestId
    requests.set(id, { resolve, reject })
    cdp.send(JSON.stringify({ id, method, params }))
  })
  const evaluate = async (expression) => {
    const result = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true })
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text)
    return result.result.value
  }
  const click = (selector, label) => until(() => evaluate(`(() => {
    const elements = Array.from(document.querySelectorAll(${JSON.stringify(selector)}));
    const text = ${JSON.stringify(label ?? null)};
    const element = text === null ? elements[0] : elements.find((el) => el.textContent.trim() === text);
    if (!element || element.disabled) return false;
    element.click();
    return true;
  })()`))
  await send('Page.enable')
  await send('Page.navigate', { url: signIn })
  await until(() => evaluate("document.querySelector('.conn.ok')?.textContent === 'live'"))
  if (await evaluate('location.hash')) throw new Error('sign-in code remained in the address bar')
  console.log('browser:', version.Browser)
  await evaluate(`window.visibleAt = {};
    (() => {
      const check = () => {
        for (const title of document.querySelectorAll('.gn-tool .gn-title')) {
          const name = Array.from(title.childNodes).find((n) => n.nodeType === Node.TEXT_NODE)?.textContent;
          if (!name?.startsWith('AtlasPerf') || name in window.visibleAt) continue;
          const node = title.closest('.react-flow__node');
          if (getComputedStyle(node).opacity !== '0' && node.getBoundingClientRect().width > 0) {
            window.visibleAt[name] = Date.now();
          }
        }
        window.requestAnimationFrame(check);
      };
      window.requestAnimationFrame(check);
    })()`)
  const samples = []
  for (let i = 0; i < 25; i++) {
    const tool = 'AtlasPerf' + i
    const raw = {
      session_id: 'perf-headless', turn_id: 't', cwd: '/workspace/perf-headless',
      hook_event_name: 'PreToolUse', model: 'perf', permission_mode: 'default',
      tool_name: tool, tool_input: {}, tool_use_id: 'perf-' + i,
    }
    const envelope = JSON.stringify({
      v: 1, envelope_id: 'perf-env-' + i, received_at: new Date().toISOString(),
      harness: 'codex', host_id: 'local', raw,
    })
    const sentAt = Date.now()
    const reply = await new Promise((resolve, reject) => {
      const conn = createConnection(join(home, 'atlasd.sock'))
      conn.setEncoding('utf8')
      let output = ''
      conn.on('connect', () => conn.write(envelope + '\n'))
      conn.on('data', (chunk) => {
        output += chunk
        if (output.includes('\n')) {
          resolve(output.trim())
          conn.destroy()
        }
      })
      conn.once('error', reject)
      conn.setTimeout(4000, () => conn.destroy(new Error('ingest socket timeout')))
    })
    if (reply !== 'ok') throw new Error('ingest reply: ' + reply)
    const result = await until(() => evaluate('window.visibleAt[' + JSON.stringify(tool) + ']'), 4000)
    samples.push(result - sentAt)
  }
  const sorted = [...samples.slice(1)].sort((a, b) => a - b)
  const percentile = (p) => sorted[Math.ceil(sorted.length * p) - 1]
  console.log('visible-render latency (warm samples):', {
    n: sorted.length, p50_ms: percentile(0.5), p95_ms: percentile(0.95),
    min_ms: sorted[0], max_ms: sorted.at(-1), first_ms: samples[0],
  })

  execFileSync(join(binaries, 'atlas'), ['hook', '--harness', 'codex'], {
    env: { ...process.env, ATLAS_HOME: home },
    input: JSON.stringify({
      session_id: 'perf-headless', turn_id: 't', cwd: '/workspace/perf-headless',
      hook_event_name: 'PostToolUse', tool_name: 'AtlasPerf24', tool_use_id: 'perf-24',
      tool_response: { exit_code: 0 },
    }),
  })
  await click('.tabs button', 'History')
  await click('.history .trow')
  await until(() => evaluate(`Array.from(document.querySelectorAll('.wf-row')).some((row) =>
    row.querySelector('.kind-tool')?.textContent === 'AtlasPerf24' && row.querySelector('.wf-bar.status-completed'))`))

  const downloads = join(dir, 'downloads')
  await mkdir(downloads)
  await send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: downloads })
  await click('.trace-head button', 'Download OTLP JSON')
  const downloaded = await until(async () => (await readdir(downloads)).find((file) => file.endsWith('.json')))
  const trace = JSON.parse(await readFile(join(downloads, downloaded), 'utf8'))
  const spans = trace.resourceSpans.flatMap((resource) => resource.scopeSpans.flatMap((scope) => scope.spans))
  if (!spans.some((span) => span.name === 'execute_tool AtlasPerf24' && span.endTimeUnixNano)) {
    throw new Error('OTLP download is missing the completed tool span')
  }

  await click('.trace-head button', 'Replay')
  const replayEvent = (event) => until(() => evaluate(`document.querySelector('.replay-current b')?.textContent === ${JSON.stringify(event)}`))
  await replayEvent('tool.completed')
  await click('.replay-controls button', '◀')
  await replayEvent('tool.started')
  await click('.replay-controls button', '▶|')
  await replayEvent('tool.completed')
  await click('.tabs button', 'Settings')
  await until(() => evaluate(`Array.from(document.querySelectorAll('.settings .cap')).some((card) => {
    if (card.querySelector('h4')?.textContent !== 'Codex') return false;
    const observed = Array.from(card.querySelectorAll('div')).find((el) => el.textContent.startsWith('Observed hooks:'));
    return observed?.textContent.includes('PreToolUse') && observed.textContent.includes('PostToolUse');
  })`))
  console.log('History, completed timeline span, OTLP download, Replay, and hook coverage passed')
} finally {
  cdp?.close()
  for (const child of [chrome, atlas]) {
    if (child?.pid && child.exitCode === null && child.signalCode === null) {
      const exited = new Promise((resolve) => child.once('exit', resolve))
      child.kill('SIGTERM')
      await Promise.race([exited, delay(2000)])
      if (child.pid && child.exitCode === null && child.signalCode === null) {
        child.kill('SIGKILL')
        await exited
      }
    }
  }
  await rm(dir, { recursive: true, force: true })
}
