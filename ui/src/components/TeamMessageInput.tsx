import {
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { Button, Icon, TextArea } from '@gravity-ui/uikit';
import { PaperPlane } from '@gravity-ui/icons';

interface Employee {
  id: string;
  name: string;
}

// Адресат — первый @тег сообщения, как и в серверной маршрутизации.
// Фокус остаётся в поле: стрелки меняют вариант, Enter только выбирает его.
export function TeamMessageInput({
  value,
  onUpdate,
  employees,
  renderAvatar,
  busy,
  canSend,
  placeholder,
}: {
  value: string;
  onUpdate: (value: string) => void;
  employees: Employee[];
  renderAvatar: (id: string) => ReactNode;
  busy: boolean;
  canSend: boolean;
  placeholder: string;
}) {
  const field = useRef<HTMLTextAreaElement>(null);
  const highlights = useRef<HTMLDivElement>(null);
  const listId = useId();
  const [caret, setCaret] = useState(0);
  const [focused, setFocused] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const [active, setActive] = useState(0);
  const query = value.slice(0, caret).match(/^@([^\s]*)$/)?.[1];
  const open =
    focused &&
    !dismissed &&
    !busy &&
    query !== undefined &&
    employees.length > 0;
  const matches = employees.filter(({ id, name }) =>
    `${id} ${name}`
      .toLocaleLowerCase()
      .includes((query || '').toLocaleLowerCase()),
  );
  const selected = Math.min(active, Math.max(0, matches.length - 1));
  const address = value.match(/^@(\S+)(?=\s|$)/)?.[1];
  const validAddress = employees.some(({ id }) => id === address);

  // Textarea сохраняет нативное редактирование, выделение и IME. Видимый текст
  // рисуется в недоступном для указателя/скринридера слое с той же геометрией.
  // clientWidth учитывает полосу прокрутки; scrollTop не даёт тегу «залипнуть».
  function syncHighlights() {
    const input = field.current;
    const mirror = highlights.current;
    if (!input || !mirror) return;
    mirror.style.width = `${input.clientWidth}px`;
    mirror.style.height = `${input.clientHeight}px`;
    mirror.scrollTop = input.scrollTop;
    mirror.scrollLeft = input.scrollLeft;
  }
  useLayoutEffect(syncHighlights, [value]);
  useEffect(() => {
    const observer = new ResizeObserver(syncHighlights);
    if (field.current) observer.observe(field.current);
    return () => observer.disconnect();
  }, []);

  // Заменяем весь редактируемый адрес, даже если курсор стоит посередине тега.
  // Остальной черновик сохраняется; пробел отделяет канонический ID от текста.
  function choose(id: string) {
    const prefix = `@${id} `;
    onUpdate(prefix + value.replace(/^@\S*\s*/, ''));
    setDismissed(true);
    requestAnimationFrame(() => {
      field.current?.focus();
      field.current?.setSelectionRange(prefix.length, prefix.length);
    });
  }

  return (
    <div className="team-compose-row">
      {open && (
        <div
          className="team-mention-menu"
          id={listId}
          role="listbox"
          aria-label="Сотрудники"
        >
          {matches.length === 0 && (
            <div className="team-mention-empty">Сотрудник не найден</div>
          )}
          {matches.map(({ id, name }, index) => (
            <Button
              key={id}
              id={`${listId}-${id}`}
              role="option"
              aria-selected={index === selected}
              aria-label={`${name} @${id}`}
              tabIndex={-1}
              view="flat"
              className="team-mention-option"
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => choose(id)}
            >
              <span className="team-mention-content">
                {renderAvatar(id)}
                <span>{name}</span>
                <span className="team-mention-id">@{id}</span>
              </span>
            </Button>
          ))}
        </div>
      )}
      <TextArea
        controlRef={field}
        controlProps={{
          'aria-label': 'Сообщение команде',
          'aria-autocomplete': 'list',
          'aria-controls': open ? listId : undefined,
          'aria-activedescendant':
            open && matches[selected]
              ? `${listId}-${matches[selected].id}`
              : undefined,
          onSelect: (event) => setCaret(event.currentTarget.selectionStart),
          onScroll: syncHighlights,
        }}
        placeholder={placeholder}
        value={value}
        onChange={(event) => {
          onUpdate(event.currentTarget.value);
          setCaret(event.currentTarget.selectionStart);
          setDismissed(false);
          setActive(0);
        }}
        onFocus={() => setFocused(true)}
        onBlur={() => setFocused(false)}
        onKeyDown={(event) => {
          if (!open || event.nativeEvent.isComposing) return;
          if (event.key === 'Escape') {
            event.preventDefault();
            event.stopPropagation();
            setDismissed(true);
          } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
            event.preventDefault();
            if (!matches.length) return;
            const next =
              (selected +
                (event.key === 'ArrowDown' ? 1 : -1) +
                matches.length) %
              matches.length;
            setActive(next);
            document
              .getElementById(`${listId}-${matches[next].id}`)
              ?.scrollIntoView?.({ block: 'nearest' });
          } else if (event.key === 'Enter') {
            event.preventDefault();
            if (matches[selected]) choose(matches[selected].id);
          }
        }}
        minRows={2}
        maxRows={4}
        disabled={busy}
      />
      <div
        ref={highlights}
        className="team-compose-highlights"
        aria-hidden="true"
      >
        {validAddress ? (
          <>
            <span className="team-compose-mention">@{address}</span>
            {value.slice(address!.length + 1)}
          </>
        ) : (
          value
        )}
        {/* Последний перенос должен занимать строку, как в textarea. */}
        {'\u200b'}
      </div>
      <Button
        className="team-compose-send"
        type="submit"
        view="action"
        size="m"
        aria-label="Отправить сообщение"
        loading={busy}
        disabled={!canSend}
      >
        <Icon data={PaperPlane} size={16} />
      </Button>
    </div>
  );
}
