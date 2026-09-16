import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';
import { CopyIdentity } from './CopyIdentity';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// Успех сообщается после записи полного ID, без отображаемого префикса.
it.each([false, true])(
  'копирует значение и показывает зелёный тост (иконка: %s)',
  async (infoIcon) => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: { writeText },
    });
    const add = vi.spyOn(toaster, 'add').mockImplementation(() => {});
    render(
      <CopyIdentity
        infoIcon={infoIcon}
        text="long-run-id"
        prefix="runId="
        label="Копировать"
        success="runId скопирован"
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Копировать' }));
    await waitFor(() =>
      expect(add).toHaveBeenCalledWith(
        expect.objectContaining({
          title: 'runId скопирован',
          theme: 'success',
        }),
      ),
    );
    expect(writeText).toHaveBeenCalledWith('long-run-id');
  },
);
