import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import hooks from 'eslint-plugin-react-hooks';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'eslint/config';

// Отдельный TypeScript анализатора не меняет версию компилятора приложения.
// Проверку совместимости с реальным компилятором выполняет отдельный tsc.
const uiRoot = fileURLToPath(new URL('../../ui/', import.meta.url));

export default defineConfig([
  {
    ignores: [
      '**/*.test.*',
      '**/*.spec.*',
      '**/test-setup.ts',
      '**/node_modules/**',
    ],
  },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      ...tseslint.configs.recommendedTypeChecked,
    ],
    languageOptions: {
      parser: tseslint.parser,
      parserOptions: { project: './tsconfig.json', tsconfigRootDir: uiRoot },
    },
    plugins: { 'react-hooks': hooks },
    rules: {
      // Типы DOM знает TypeScript; дублирующий no-undef даёт ложные ошибки.
      'no-undef': 'off',
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      complexity: ['warn', 15],
    },
  },
]);
