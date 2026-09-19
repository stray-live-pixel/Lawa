import { it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import * as icons from '@gravity-ui/icons';

// Обновление npm-пакета не должно рассинхронизировать CLI-валидацию и UI.
it('каталог Go соответствует экспортам установленной Gravity UI', () => {
  const names = readFileSync('../internal/workflow/graph-icons.txt', 'utf8')
    .trim()
    .split('\n');
  expect(names).toEqual(Object.keys(icons).sort());
});
