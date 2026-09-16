import { useState } from 'react';
import { Button, Icon } from '@gravity-ui/uikit';
import {
  ChevronsExpandUpRight,
  ChevronsCollapseUpRight,
} from '@gravity-ui/icons';

// Кнопка принадлежит строке вкладок, поэтому не исчезает при смене вкладки.
// Gravity формирует aria-pressed из selected: этот атрибут связывает
// доступное состояние кнопки и CSS-сетку (напрямую задавать его нельзя).
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
      selected={expanded}
      onClick={() => setExpanded(!expanded)}
    >
      <Icon data={expanded ? ChevronsCollapseUpRight : ChevronsExpandUpRight} />
    </Button>
  );
}
