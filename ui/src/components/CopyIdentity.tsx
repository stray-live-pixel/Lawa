import { CircleInfo } from '@gravity-ui/icons';
import {
  Button,
  ClipboardButton,
  CopyToClipboard,
  Icon,
  Tooltip,
} from '@gravity-ui/uikit';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';

// Копируем полное значение, даже когда CSS обрезает видимую строку.
// Успешный тост появляется только после подтверждения записи в буфер.
export function CopyIdentity({
  text,
  label,
  success,
  prefix = '',
  infoIcon = false,
}: {
  text: string;
  label: string;
  success: string;
  prefix?: string;
  infoIcon?: boolean;
}) {
  const onCopy = (_: string, copied: boolean) => {
    toaster.remove('copy-workflow-identity');
    toaster.add({
      name: 'copy-workflow-identity',
      title: copied ? success : 'Не удалось скопировать',
      theme: copied ? 'success' : 'danger',
      autoHiding: 2500,
    });
  };
  if (infoIcon) {
    return (
      <Tooltip content={`Run ID = ${text}`}>
        <span className="run-id-trigger">
          <ClipboardButton
            className="run-id-info"
            view="flat"
            size="s"
            text={text}
            icon={<Icon data={CircleInfo} size={18} />}
            aria-label={label}
            hasTooltip={false}
            onCopy={onCopy}
          />
        </span>
      </Tooltip>
    );
  }
  return (
    <CopyToClipboard text={text} onCopy={onCopy}>
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
