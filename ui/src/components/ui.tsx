import { useId, type ReactNode } from 'react';
import {
  Button as GravityButton,
  Modal,
  Icon,
  Label,
  Alert,
  DefinitionList,
  Select,
  type ButtonProps,
  type SelectOption,
} from '@gravity-ui/uikit';
import { Xmark } from '@gravity-ui/icons';

// Общие элементы делегируют оформление и доступность Gravity UI. Обёртки
// сохраняют только соглашения приложения: destructive-view и одиночный выбор.
export function Button({ className = '', ...props }: ButtonProps) {
  return (
    <GravityButton
      view={className.includes('danger') ? 'outlined-danger' : 'outlined'}
      className={className}
      {...props}
    />
  );
}
export function Choice({
  value,
  onUpdate,
  options,
  ...props
}: {
  value: string;
  onUpdate: (value: string) => void;
  options: SelectOption[];
  'aria-label': string;
  className?: string;
}) {
  return (
    <Select
      {...props}
      value={value ? [value] : []}
      options={options}
      onUpdate={([next]) => {
        if (next !== undefined) onUpdate(next);
      }}
    />
  );
}
// Modal обеспечивает portal, focus trap, Escape и возврат фокуса инициатору.
// Заголовок и описание явно связаны с диалогом для screen reader.
export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: string;
  children: ReactNode;
}) {
  const titleID = useId(),
    descriptionID = useId();
  return (
    <Modal
      open={open}
      onOpenChange={onOpenChange}
      aria-labelledby={titleID}
      aria-describedby={description ? descriptionID : undefined}
      contentClassName="dialog-content"
    >
      <div className="dialog-header">
        <h2 id={titleID}>{title}</h2>
        <GravityButton
          view="flat"
          aria-label="Закрыть"
          onClick={() => onOpenChange(false)}
        >
          <Icon data={Xmark} size={18} />
        </GravityButton>
      </div>
      {description && (
        <p id={descriptionID} className="muted">
          {description}
        </p>
      )}
      <div className="dialog-body">{children}</div>
    </Modal>
  );
}
export const statusNames: Record<string, string> = {
  pending: 'Ожидает запуска',
  starting: 'Запускается',
  running: 'В работе',
  waiting_for_approval: 'Ожидает подтверждения',
  succeeded: 'Готово',
  failed: 'Ошибка',
  cancelled: 'Отменено',
  interrupted: 'Прервано',
  skipped: 'Пропущено',
  unknown: 'Неизвестно',
};
// Подпись состояния сохраняется независимо от цвета: тема не меняет смысл.
export function Status({ state }: { state: string }) {
  const theme =
    state === 'succeeded'
      ? 'success'
      : ['running', 'starting', 'waiting_for_approval'].includes(state)
        ? 'info'
        : ['failed', 'cancelled'].includes(state)
          ? 'danger'
          : 'normal';
  return (
    <Label theme={theme} size="s">
      {statusNames[state] || state}
    </Label>
  );
}
export function ErrorNotice({ error }: { error?: string }) {
  return error ? (
    <div role="alert">
      <Alert theme="danger" message={error} />
    </div>
  ) : null;
}
export function Facts({
  items,
}: {
  items: [string, string | number | undefined][];
}) {
  return (
    <DefinitionList className="facts" responsive>
      {items
        .filter(([, value]) => value !== '' && value !== undefined)
        .map(([label, value]) => (
          <DefinitionList.Item key={label} name={label}>
            {value}
          </DefinitionList.Item>
        ))}
    </DefinitionList>
  );
}
