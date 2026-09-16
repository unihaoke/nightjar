/**
 * 前端构建产物冒烟检查（无需人工点页面）。
 *
 * 背景：曾出现「构建成功、类型检查通过，但浏览器白屏」的问题——手工分包造成
 * element-plus 与 dayjs 所在的 chunk 互相循环依赖，触发 ES module TDZ：
 *   Uncaught ReferenceError: Cannot access 'Pt' before initialization
 * 该错误只在浏览器运行时暴露，`npm run build` 与 `vue-tsc` 都发现不了。
 * 因此把「加载产物页面 → 捕获 console 错误与未捕获异常 → 断言 #app 已渲染」固化成本脚本。
 *
 * 用法：
 *   node scripts/frontend-smoke.cjs                      # 默认 http://127.0.0.1:8099/
 *   node scripts/frontend-smoke.cjs http://host:8000/    # 检查任意已部署地址
 *   node scripts/frontend-smoke.cjs --self-serve         # 用内置静态服务器托管 dist/ 后自测
 *
 * 退出码：0 = 无运行时错误且 #app 已渲染；1 = 检出运行时错误；2 = 脚本自身异常。
 */
const { spawn } = require('node:child_process')
const http = require('node:http')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')

const rawArgs = process.argv.slice(2)
const selfServe = rawArgs.includes('--self-serve')
const urlArg = rawArgs.find((a) => !a.startsWith('--'))
const targetUrl = urlArg || 'http://127.0.0.1:8099/'
const debugPort = Number(process.env.SMOKE_CDP_PORT || 9333)

const WEB_ROOT = path.resolve(__dirname, '..')
const DIST_DIR = path.join(WEB_ROOT, 'dist')

const BROWSER_CANDIDATES = [
  process.env.CHROME_PATH,
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
].filter(Boolean)

function findBrowser() {
  for (const candidate of BROWSER_CANDIDATES) {
    if (fs.existsSync(candidate)) {
      return candidate
    }
  }
  return null
}

/** 启动内置静态服务器托管 dist/，返回 { server, port }。 */
function startStaticServer() {
  return new Promise((resolve, reject) => {
    if (!fs.existsSync(DIST_DIR)) {
      reject(new Error(`未找到构建产物目录 ${DIST_DIR}，请先执行 npm run build`))
      return
    }
    const mime = {
      '.html': 'text/html; charset=utf-8',
      '.js': 'text/javascript; charset=utf-8',
      '.css': 'text/css; charset=utf-8',
      '.svg': 'image/svg+xml',
      '.json': 'application/json',
      '.woff2': 'font/woff2',
    }
    const server = http.createServer((req, res) => {
      let pathname = decodeURIComponent((req.url || '/').split('?')[0])
      if (pathname === '/') {
        pathname = '/index.html'
      }
      const file = path.join(DIST_DIR, pathname)
      fs.readFile(file, (err, data) => {
        if (err) {
          res.writeHead(404, { 'Content-Type': 'text/plain' })
          res.end('not found')
          return
        }
        res.writeHead(200, { 'Content-Type': mime[path.extname(file)] || 'application/octet-stream' })
        res.end(data)
      })
    })
    server.on('error', reject)
    server.listen(0, '127.0.0.1', () => resolve({ server, port: server.address().port }))
  })
}

function httpJson(port, pathname) {
  return new Promise((resolve, reject) => {
    const req = http.get({ host: '127.0.0.1', port, path: pathname }, (res) => {
      let body = ''
      res.on('data', (c) => (body += c))
      res.on('end', () => {
        try {
          resolve(JSON.parse(body))
        } catch (e) {
          reject(e)
        }
      })
    })
    req.on('error', reject)
    req.setTimeout(5000, () => req.destroy(new Error('devtools http timeout')))
  })
}

async function waitForDevTools(port, retries = 60) {
  for (let i = 0; i < retries; i++) {
    try {
      return await httpJson(port, '/json/version')
    } catch {
      await new Promise((r) => setTimeout(r, 250))
    }
  }
  throw new Error('DevTools 端口未就绪')
}

/** 极简 WebSocket 客户端：只需支持 CDP 的文本帧收发。 */
class WS {
  constructor(socket) {
    this.socket = socket
    this.buffer = Buffer.alloc(0)
    this.handlers = new Map()
    this.id = 0
    socket.on('data', (chunk) => this.onData(chunk))
    // 浏览器退出导致的断开属于预期，不应抛未处理错误。
    socket.on('error', () => {})
  }

  static connect(wsUrl) {
    return new Promise((resolve, reject) => {
      const u = new URL(wsUrl)
      const req = http.request({
        host: u.hostname,
        port: u.port,
        path: u.pathname + (u.search || ''),
        headers: {
          Connection: 'Upgrade',
          Upgrade: 'websocket',
          'Sec-WebSocket-Key': Buffer.from(String(Math.random())).toString('base64'),
          'Sec-WebSocket-Version': '13',
        },
      })
      req.on('upgrade', (_res, socket) => resolve(new WS(socket)))
      req.on('error', reject)
      req.end()
    })
  }

  onData(chunk) {
    this.buffer = Buffer.concat([this.buffer, chunk])
    for (;;) {
      if (this.buffer.length < 2) return
      const b1 = this.buffer[1]
      const masked = (b1 & 0x80) !== 0
      let len = b1 & 0x7f
      let offset = 2
      if (len === 126) {
        if (this.buffer.length < 4) return
        len = this.buffer.readUInt16BE(2)
        offset = 4
      } else if (len === 127) {
        if (this.buffer.length < 10) return
        len = Number(this.buffer.readBigUInt64BE(2))
        offset = 10
      }
      const maskLen = masked ? 4 : 0
      if (this.buffer.length < offset + maskLen + len) return
      const payload = this.buffer.subarray(offset + maskLen, offset + maskLen + len)
      this.buffer = this.buffer.subarray(offset + maskLen + len)
      let message
      try {
        message = JSON.parse(payload.toString('utf8'))
      } catch {
        continue
      }
      if (message.id && this.handlers.has(message.id)) {
        this.handlers.get(message.id)(message)
        this.handlers.delete(message.id)
      } else if (message.method && this.onEvent) {
        this.onEvent(message)
      }
    }
  }

  send(method, params) {
    const id = ++this.id
    const payload = Buffer.from(JSON.stringify({ id, method, params: params || {} }))
    const mask = Buffer.from([1, 2, 3, 4])
    const len = payload.length
    let header
    if (len < 126) {
      header = Buffer.from([0x81, 0x80 | len])
    } else if (len < 65536) {
      header = Buffer.alloc(4)
      header[0] = 0x81
      header[1] = 0x80 | 126
      header.writeUInt16BE(len, 2)
    } else {
      header = Buffer.alloc(10)
      header[0] = 0x81
      header[1] = 0x80 | 127
      header.writeBigUInt64BE(BigInt(len), 2)
    }
    const masked = Buffer.alloc(len)
    for (let i = 0; i < len; i++) {
      masked[i] = payload[i] ^ mask[i % 4]
    }
    this.socket.write(Buffer.concat([header, mask, masked]))
    return new Promise((resolve) => this.handlers.set(id, resolve))
  }

  close() {
    try {
      this.socket.destroy()
    } catch {
      /* 忽略 */
    }
  }
}

async function main() {
  const browser = findBrowser()
  if (!browser) {
    console.error('跳过：未找到 Chrome/Edge，可在环境变量 CHROME_PATH 指定浏览器路径')
    process.exit(0)
  }

  let staticServer = null
  let target = targetUrl
  if (selfServe) {
    const started = await startStaticServer()
    staticServer = started.server
    target = `http://127.0.0.1:${started.port}/`
    console.log(`已启动内置静态服务器: ${target}`)
  }
  console.log(`检查目标: ${target}`)

  const profile = path.join(os.tmpdir(), `mwops-smoke-${Date.now()}`)
  const proc = spawn(
    browser,
    [
      '--headless=new',
      '--disable-gpu',
      '--no-first-run',
      '--no-default-browser-check',
      '--disable-extensions',
      '--hide-scrollbars',
      '--mute-audio',
      `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${profile}`,
      'about:blank',
    ],
    { stdio: 'ignore' },
  )

  const errors = []
  let ws = null
  try {
    await waitForDevTools(debugPort)
    const targets = await httpJson(debugPort, '/json/list')
    const page = targets.find((t) => t.type === 'page')
    if (!page) {
      throw new Error('未找到 page target')
    }

    ws = await WS.connect(page.webSocketDebuggerUrl)
    ws.onEvent = (msg) => {
      if (msg.method === 'Runtime.consoleAPICalled') {
        const text = (msg.params.args || []).map((a) => a.value ?? a.description ?? '').join(' ')
        if (msg.params.type === 'error') {
          errors.push(`[console.error] ${text}`)
        }
      }
      if (msg.method === 'Runtime.exceptionThrown') {
        const details = msg.params.exceptionDetails
        errors.push(`[uncaught] ${details.exception?.description || details.text}`)
      }
    }

    await ws.send('Runtime.enable')
    await ws.send('Page.enable')
    await ws.send('Page.navigate', { url: target })
    await new Promise((r) => setTimeout(r, 6000))

    const evaluated = await ws.send('Runtime.evaluate', {
      expression: `JSON.stringify({
        rendered: (document.querySelector('#app')?.innerHTML || '').length,
        title: document.title
      })`,
      returnByValue: true,
    })
    const state = JSON.parse(evaluated.result?.result?.value || '{}')
    const rendered = Number(state.rendered || 0)

    console.log(`页面标题: ${state.title || '(空)'}`)
    console.log(`#app 渲染长度: ${rendered}`)
    console.log(`运行时错误: ${errors.length} 条`)
    errors.slice(0, 10).forEach((e) => console.log('  ' + e))

    const failed = errors.length > 0 || rendered === 0
    if (failed) {
      if (rendered === 0 && errors.length === 0) {
        console.error('失败：#app 为空且未捕获到错误（应用未挂载）')
      } else {
        console.error('失败：检出前端运行时错误')
      }
      process.exitCode = 1
    } else {
      console.log('通过：应用已挂载且无运行时错误')
      process.exitCode = 0
    }
  } finally {
    if (ws) {
      ws.close()
    }
    proc.kill()
    await new Promise((r) => setTimeout(r, 300))
    if (staticServer) {
      staticServer.close()
    }
    fs.rmSync(profile, { recursive: true, force: true })
  }
}

main().catch((e) => {
  console.error('冒烟脚本自身异常:', e.message)
  process.exit(2)
})
