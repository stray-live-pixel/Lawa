import { Icon, Tooltip } from '@gravity-ui/uikit';
import {
  CircleDashed,
  Clock,
  ArrowRotateRight,
  CirclePlay,
  HandStop,
  CircleCheck,
  CircleExclamation,
  CircleXmark,
  CirclePause,
  CircleMinus,
  CircleQuestion,
} from '@gravity-ui/icons';
import { statusNames } from './ui';

// Компактное представление только для панели деталей. Форма различает статусы
// без опоры на цвет; текст сохраняется в подсказке и доступном имени.
const icons = {
  not_started: CircleDashed,
  pending: Clock,
  starting: ArrowRotateRight,
  running: CirclePlay,
  waiting_for_approval: HandStop,
  succeeded: CircleCheck,
  failed: CircleExclamation,
  cancelled: CircleXmark,
  interrupted: CirclePause,
  skipped: CircleMinus,
  unknown: CircleQuestion,
};
export function StatusIcon({ state }: { state: string }) {
  const label = statusNames[state] || state;
  return (
    <Tooltip content={label}>
      <span
        className={`status-icon tone-${state}`}
        role="img"
        aria-label={label}
        tabIndex={0}
      >
        <Icon
          data={icons[state as keyof typeof icons] || CircleQuestion}
          size={18}
        />
      </span>
    </Tooltip>
  );
}
