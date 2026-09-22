// fixcjs.mjs — CJS 产物重命名为 .cjs（父级 package.json 有 "type": "module"，
// .js 会被 Node 当 ESM 解析，require() 互操作拿到空命名空间），并修正内部
// require 指向与生成对应 .d.cts 声明名。
import { readdirSync, renameSync, readFileSync, writeFileSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist', 'cjs');

for (const f of readdirSync(dir)) {
  if (f.endsWith('.js')) {
    renameSync(join(dir, f), join(dir, f.replace(/\.js$/, '.cjs')));
  } else if (f.endsWith('.d.ts')) {
    renameSync(join(dir, f), join(dir, f.replace(/\.d\.ts$/, '.d.cts')));
  }
}
for (const f of readdirSync(dir)) {
  if (f.endsWith('.cjs')) {
    const p = join(dir, f);
    // 内部相对 require 指向 .cjs；声明引用指向 .d.cts
    writeFileSync(p, readFileSync(p, 'utf8').replace(/require\("\.\/(types)\.js"\)/g, 'require("./$1.cjs")'));
  }
}
console.log('cjs 产物已重命名为 .cjs');
