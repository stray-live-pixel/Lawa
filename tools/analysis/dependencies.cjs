// Только production-импорты UI. Границы слоёв задаются явно, без угадывания
// архитектуры по именам директорий; пока запрещены циклы и неразрешимые импорты.
module.exports = {
  forbidden: [
    {
      name: 'no-circular',
      severity: 'error',
      from: {},
      to: { circular: true },
    },
    {
      name: 'no-unresolved',
      severity: 'error',
      from: {},
      to: { couldNotResolve: true },
    },
  ],
  options: {
    doNotFollow: { path: 'node_modules' },
    exclude: { path: '\\.(test|spec)\\.|test-setup\\.ts$' },
    tsConfig: { fileName: 'tsconfig.json' },
    // Vite читает package.json exports; без exportsFields допустимые subpath-
    // импорты UIKit ошибочно отмечаются как неразрешимые.
    enhancedResolveOptions: {
      extensions: ['.ts', '.tsx', '.js', '.jsx', '.json'],
      exportsFields: ['exports'],
      conditionNames: ['import', 'browser', 'default'],
    },
  },
};
