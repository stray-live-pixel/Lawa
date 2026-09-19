// Канонические имена Gravity общие для строгого Go-валидатора и интерфейса.
// После обновления @gravity-ui/icons: node scripts/generate-graph-icons.mjs.
import { readFileSync, writeFileSync } from 'node:fs';
const source = readFileSync(
  new URL('../node_modules/@gravity-ui/icons/index.d.ts', import.meta.url),
  'utf8',
);
const names = [...source.matchAll(/default as (\w+)/g)]
  .map((match) => match[1])
  .sort();
writeFileSync(
  new URL('../../internal/workflow/graph-icons.txt', import.meta.url),
  names.join('\n') + '\n',
);
