import { Button, CopyToClipboard } from '@gravity-ui/uikit';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';

// Копируем полное значение, даже когда CSS обрезает видимую строку.
// Успешный тост появляется только после подтверждения записи в буфер.
export function CopyIdentity({
  text,
  label,
  success,
  prefix = '',
}: {
  text: string;
  label: string;
  success: string;
  prefix?: string;
}) {
  return (
    <CopyToClipboard
      text={text}
      onCopy={(_, copied) => {
        toaster.remove('copy-workflow-identity');
        toaster.add({
          name: 'copy-workflow-identity',
          title: copied ? success : 'Не удалось скопировать',
          theme: copied ? 'success' : 'danger',
          autoHiding: 2500,
        });
      }}
    >
      <Button
        view="flat"
        className="copy-identity"
        aria-label={label}
        title={prefix + text}
      >
        {prefix}
        {text}
      </Button>
    </CopyToClipboard>
  );
}
