import { Icon, Tooltip } from '@gravity-ui/uikit';
import { Cube, NodesRight } from '@gravity-ui/icons';
import { statusNames } from './ui';

// Форма обозначает сущность, цвет — состояние. Подсказка и доступное имя
// передают оба значения независимо от восприятия цвета. Только панель деталей.
export function StatusIcon({
  state,
  entity,
}: {
  state: string;
  entity: 'workflow' | 'cube';
}) {
  const stateLabel =
    entity === 'workflow' && state === 'cancelled'
      ? 'Остановлено'
      : statusNames[state] || state;
  const label = `${entity === 'workflow' ? 'Workflow' : 'Кубик'}. ${stateLabel}`;
  return (
    <Tooltip content={label}>
      <span
        className={`status-icon tone-${state}`}
        role="img"
        aria-label={label}
        tabIndex={0}
      >
        <Icon data={entity === 'workflow' ? NodesRight : Cube} size={18} />
      </span>
    </Tooltip>
  );
}
