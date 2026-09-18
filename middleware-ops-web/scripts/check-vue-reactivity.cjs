#!/usr/bin/env node
/**
 * 前端「按 key 绑定表单」的响应式检查（静态，无需浏览器）。
 *
 * 背景（INC-020）：AI 设置页与通知渠道页都用
 *
 *     const providers = { third_party: {...}, self_hosted: {...} }   // ← 普通对象
 *     <el-switch v-model="providers[item.key].enabled" />
 *
 * 这种"遍历渲染 + 按 key 取子对象再 v-model"的写法。普通对象赋值会成功（保存时读到的是新值），
 * 但**不会触发视图重渲染**：表现就是"启用开关点不动、协议下拉选了没反应"。
 * 类型检查与打包都发现不了——它既不是类型错误，也不是模块错误，只有"点一下页面"才暴露。
 *
 * 因此把这条约定固化成检查：凡是模板里出现 `v-model="X[...]..."`，同一文件里
 * X 的声明必须用 reactive / ref / shallowReactive / shallowRef / toRef 包起来。
 *
 * 用法：node scripts/check-vue-reactivity.cjs [src目录]
 * 退出码：0 = 全部合规；1 = 检出问题（并给出文件、变量与改法）。
 */
const fs = require('node:fs')
const path = require('node:path')

const SRC_DIR = path.resolve(process.argv[2] || path.join(__dirname, '..', 'src'))

/** 允许的响应式包装函数。 */
const REACTIVE_WRAPPERS = ['reactive', 'shallowReactive', 'ref', 'shallowRef', 'toRef', 'customRef']

/** 递归收集 .vue 文件。 */
function collectVueFiles(dir) {
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) {
      out.push(...collectVueFiles(full))
    } else if (entry.isFile() && entry.name.endsWith('.vue')) {
      out.push(full)
    }
  }
  return out
}

/** 找出模板里 `v-model="X[...]"` 的目标变量名（去重，保留首次出现位置）。 */
function indexedVModelTargets(content) {
  const found = new Map()
  const re = /v-model(?::[\w-]+)?="\s*([A-Za-z_$][\w$]*)\s*\[/g
  let match
  while ((match = re.exec(content)) !== null) {
    const name = match[1]
    if (!found.has(name)) {
      found.set(name, content.slice(0, match.index).split('\n').length)
    }
  }
  return found
}

/**
 * 取变量声明右侧的文本，用于判断是否被响应式包装。
 *
 * 返回 null 表示没找到声明；返回以 `{` / `[` 开头的文本表示是普通对象/数组字面量（即本检查的目标）。
 */
function declarationRHS(content, name) {
  const re = new RegExp(`(?:const|let|var)\\s+${name}\\s*(?::[^=\\n]+)?=\\s*([^\\n]*)`)
  const match = content.match(re)
  return match ? match[1].trim() : null
}

function main() {
  if (!fs.existsSync(SRC_DIR)) {
    console.error(`检查目录不存在：${SRC_DIR}`)
    process.exit(2)
  }

  const problems = []
  const unconfirmed = []
  const checkedNames = []
  for (const file of collectVueFiles(SRC_DIR)) {
    const content = fs.readFileSync(file, 'utf8')
    const targets = indexedVModelTargets(content)
    for (const [name, line] of targets) {
      checkedNames.push(`${path.relative(process.cwd(), file)}:${line} ${name}`)
      const rhs = declarationRHS(content, name)
      if (rhs === null) {
        problems.push({
          file,
          line,
          message: `v-model 绑定了 ${name}[...]，但在本文件里找不到 ${name} 的声明，无法确认它是否响应式`,
        })
        continue
      }
      if (REACTIVE_WRAPPERS.some((wrapper) => rhs.startsWith(wrapper))) {
        continue
      }
      if (rhs.startsWith('{') || rhs.startsWith('[')) {
        problems.push({
          file,
          line,
          message:
            `v-model 绑定了 ${name}[...]，而 ${name} 是普通对象/数组字面量。` +
            `改成 const ${name} = reactive<...>({...})：普通对象的赋值会成功但视图不更新（INC-020）`,
        })
        continue
      }
      // 其它表达式（例如工厂函数返回值）无法静态判定，只提示不失败，避免误报。
      unconfirmed.push({ file, line, name, rhs: rhs.split('(')[0] })
    }
  }

  const relative = (f) => path.relative(process.cwd(), f)
  for (const item of unconfirmed) {
    console.warn(
      `  [待人工确认] ${relative(item.file)}:${item.line}  ${item.name} = ${item.rhs}(...)` +
        `：请确认它返回的是 reactive/ref 对象`,
    )
  }
  if (problems.length > 0) {
    console.error(`前端响应式检查未通过（检查了 ${checkedNames.length} 个按 key 绑定的 v-model 变量）：`)
    for (const p of problems) {
      console.error(`  ${relative(p.file)}:${p.line}  ${p.message}`)
    }
    process.exit(1)
  }
  console.log(
    `前端响应式检查通过：${checkedNames.length} 个按 key 绑定的 v-model 变量均为响应式声明` +
      (unconfirmed.length > 0 ? `（另有 ${unconfirmed.length} 个待人工确认，见上）` : ''),
  )
}

main()
