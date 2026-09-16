import { useState } from 'react';
import { Button, Icon } from '@gravity-ui/uikit';
import {
  ChevronsExpandUpRight,
  ChevronsCollapseUpRight,
} from '@gravity-ui/icons';

// Кнопка принадлежит строке вкладок, поэтому не исчезает при смене вкладки.
// aria-pressed одновременно описывает состояние для пользователя и CSS-сетки:
// меню скрывается без размонтирования и сохраняет выбранную ширину.
export function SidebarToggle() {
  const [expanded, setExpanded] = useState(false);
  const label = expanded ? 'Свернуть граф' : 'Развернуть граф';
  return (
    <Button
      className="sidebar-toggle"
      view="flat"
      size="s"
      aria-label={label}
      title={label}
      aria-pressed={expanded}
      onClick={() => setExpanded(!expanded)}
    >
      <Icon data={expanded ? ChevronsCollapseUpRight : ChevronsExpandUpRight} />
    </Button>
  );
}
