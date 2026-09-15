import { useEffect, useState } from 'react';

// HTTP-ошибка возвращается пользователю текстом, не вставляется как HTML.
export async function request<T>(
  url: string,
  signal?: AbortSignal,
  format: 'json' | 'text' = 'json',
): Promise<T> {
  const response = await fetch(url, { signal, cache: 'no-store' });
  if (!response.ok)
    throw new Error(
      (await response.text()).trim() || `HTTP ${response.status}`,
    );
  return (format === 'text' ? response.text() : response.json()) as Promise<T>;
}

// Только один запрос на ресурс одновременно. Смена URL немедленно скрывает
// чужие данные, abort и проверка disposed запрещают позднему ответу перезапись.
// При временном сбое сохраняем последний снимок того же ресурса и показываем ошибку.
export function usePoll<T>(
  url: string | null,
  interval = 3000,
  format: 'json' | 'text' = 'json',
) {
  const [state, setState] = useState<{
    url: string | null;
    data?: T;
    error?: string;
  }>({ url });
  useEffect(() => {
    if (!url) return;
    const controller = new AbortController();
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const data = await request<T>(url, controller.signal, format);
        if (!disposed) setState({ url, data });
      } catch (error) {
        if (!disposed)
          setState((previous) => ({
            url,
            data: previous.url === url ? previous.data : undefined,
            error: String(error),
          }));
      } finally {
        if (!disposed && interval > 0) timer = setTimeout(load, interval);
      }
    };
    void load();
    return () => {
      disposed = true;
      controller.abort();
      clearTimeout(timer);
    };
  }, [url, interval, format]);
  return state.url === url ? state : { url };
}
