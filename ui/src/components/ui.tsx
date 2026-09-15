import {
  useId,
  useRef,
  type ButtonHTMLAttributes,
  type ReactNode,
} from 'react';
import * as DialogPrimitive from '@radix-ui/react-dialog';
import { X } from 'lucide-react';

// Небольшой слой компонентов изолирует приложение от конкретной библиотеки.
// Тему меняют CSS-токены, а focus trap, Escape и возврат фокуса обеспечивает Radix.
export function Button({
  className = '',
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement>) {
  return <button type="button" className={`button ${className}`} {...props} />;
}
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
  const descriptionID = useId();
  // Один управляемый диалог открывается разными кнопками, без Radix Trigger.
  // Сохраняем инициатор до auto-focus и возвращаем фокус, только если он ещё в DOM.
  const initiator = useRef<HTMLElement | null>(null);
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay className="dialog-overlay" />
        <DialogPrimitive.Content
          className="dialog-content"
          aria-describedby={description ? descriptionID : undefined}
          onOpenAutoFocus={() => {
            initiator.current =
              document.activeElement instanceof HTMLElement
                ? document.activeElement
                : null;
          }}
          onCloseAutoFocus={(event) => {
            event.preventDefault();
            if (initiator.current?.isConnected) initiator.current.focus();
          }}
        >
          <div className="dialog-header">
            <DialogPrimitive.Title>{title}</DialogPrimitive.Title>
            <DialogPrimitive.Close asChild>
              <Button aria-label="Закрыть">
                <X size={18} />
              </Button>
            </DialogPrimitive.Close>
          </div>
          {description && (
            <DialogPrimitive.Description id={descriptionID} className="muted">
              {description}
            </DialogPrimitive.Description>
          )}
          <div className="dialog-body">{children}</div>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
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
export function Status({ state }: { state: string }) {
  return (
    <span className={`status tone-${state}`}>
      {statusNames[state] || state}
    </span>
  );
}
export function ErrorNotice({ error }: { error?: string }) {
  return error ? (
    <p className="error" role="alert">
      {error}
    </p>
  ) : null;
}
export function Facts({
  items,
}: {
  items: [string, string | number | undefined][];
}) {
  return (
    <dl className="facts">
      {items
        .filter(([, value]) => value !== '' && value !== undefined)
        .map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
    </dl>
  );
}
