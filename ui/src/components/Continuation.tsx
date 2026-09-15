import { useRef, useState } from 'react';
import { Button } from './ui';

// Поле остаётся обычным текстом: промпт можно прочитать и скопировать вручную,
// в том числе в браузере без Clipboard API или на несекьюрном HTTP origin.
export function Continuation({
  cube,
  workflow,
}: {
  cube: string;
  workflow: string;
}) {
  const [scope, setScope] = useState('cube');
  const [copied, setCopied] = useState('');
  const field = useRef<HTMLTextAreaElement>(null);
  const prompt = scope === 'workflow' ? workflow : cube;
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(prompt);
      setCopied('Скопировано. Добавьте задачу в новом чате.');
    } catch {
      field.current?.focus();
      field.current?.select();
      setCopied('Скопируйте выделенный текст: Ctrl+C или ⌘C.');
    }
  };
  return (
    <section>
      <h3>Продолжить в новом чате Codex</h3>
      <select
        aria-label="Контекст продолжения"
        value={scope}
        onChange={(event) => {
          setScope(event.target.value);
          setCopied('');
        }}
      >
        <option value="cube">Этот кубик</option>
        <option value="workflow">Весь workflow</option>
      </select>
      <textarea
        ref={field}
        readOnly
        aria-label="Промпт продолжения"
        value={prompt}
      />
      <Button onClick={() => void copy()}>Скопировать промпт</Button>
      <p role="status" className="muted">
        {copied}
      </p>
    </section>
  );
}
