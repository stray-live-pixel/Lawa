import { useRef, useState } from 'react';
import { Button, Text } from '@gravity-ui/uikit';
import { ErrorNotice } from './ui';

export interface TeamCharacter {
  name: string;
  history: string;
  instructions: string;
  avatar?: string;
}
export type TeamCharacters = Record<string, TeamCharacter>;

// Импортируется только каталог личностей. Права и маршрутизация сообщений
// остаются на сервере; ошибочный файл не должен незаметно запускать обычную команду.
export function parseTeamConfig(text: string): TeamCharacters {
  const config = JSON.parse(text);
  if (
    !config ||
    Object.keys(config).some((key) => key !== 'characters') ||
    !config.characters ||
    typeof config.characters !== 'object' ||
    Array.isArray(config.characters)
  )
    throw new Error('Нужен JSON-объект с полем characters');
  const entries = Object.entries(config.characters);
  if (entries.length + (Object.hasOwn(config.characters, 'boss') ? 0 : 1) > 20)
    throw new Error('В команде может быть до 20 личностей, включая Босса');
  for (const [id, value] of entries) {
    const c = value as TeamCharacter;
    if (
      !/^[a-z][a-z0-9_-]{0,63}$/.test(id) ||
      id === 'human' ||
      id === 'system' ||
      !c ||
      typeof c !== 'object' ||
      Array.isArray(c) ||
      Object.keys(c).some(
        (key) => !['name', 'history', 'instructions', 'avatar'].includes(key),
      ) ||
      [c.name, c.history, c.instructions].some(
        (v) => typeof v !== 'string' || !v.trim(),
      ) ||
      (c.avatar !== undefined &&
        (typeof c.avatar !== 'string' ||
          !/^[a-z][a-z0-9_-]{0,63}$/.test(c.avatar)))
    )
      throw new Error(
        `Проверьте characters.${id}: нужны безопасный ID, name, history, instructions и необязательный avatar`,
      );
  }
  return config.characters;
}

// undefined означает команду по умолчанию; пустой объект — только Босс.
// Ошибка очищает прежний выбор и блокирует создание, пока файл не исправят/сбросят.
export function TeamConfigInput({
  disabled,
  onChange,
}: {
  disabled: boolean;
  onChange: (characters: TeamCharacters | undefined, valid: boolean) => void;
}) {
  const input = useRef<HTMLInputElement>(null);
  const [name, setName] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  async function load(file: File) {
    setName(file.name);
    setLoading(true);
    setError('');
    onChange(undefined, false);
    try {
      if (file.size > 96 * 1024)
        throw new Error('Конфиг должен быть не больше 96 КБ');
      onChange(parseTeamConfig(await file.text()), true);
    } catch (cause) {
      setError(String(cause));
    } finally {
      setLoading(false);
    }
  }
  return (
    <div>
      <input
        ref={input}
        type="file"
        accept=".json,application/json"
        hidden
        aria-label="JSON команды"
        disabled={disabled || loading}
        onChange={(event) => {
          const file = event.target.files?.[0];
          if (file) void load(file);
          event.target.value = '';
        }}
      />
      <Button
        disabled={disabled || loading}
        loading={loading}
        onClick={() => input.current?.click()}
      >
        Загрузить состав команды
      </Button>{' '}
      {name && (
        <Button
          disabled={disabled || loading}
          view="flat"
          onClick={() => {
            setName('');
            setError('');
            onChange(undefined, true);
          }}
        >
          Сбросить
        </Button>
      )}
      <Text as="div" variant="caption-2" color="secondary">
        {name || 'Без JSON: Босс и Разработчик. Остальных приглашает Босс.'}
      </Text>
      <ErrorNotice error={error} />
    </div>
  );
}
