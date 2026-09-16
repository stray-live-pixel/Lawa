import '@testing-library/jest-dom/vitest';

// jsdom не рассчитывает геометрию и media queries. Это только браузерные API,
// сами компоненты Gravity остаются настоящими для проверки действий и ARIA.
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (media: string) => ({
    matches: false,
    media,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
  }),
});
class TestResizeObserver {
  observe() {}
  unobserve() {}
  disconnect() {}
}
globalThis.ResizeObserver = TestResizeObserver;
Element.prototype.scrollIntoView = () => {};
