import { Icon } from '@gravity-ui/uikit';
import * as icons from '@gravity-ui/icons';

// Только экспорты пакета: конфигурация не может подставить SVG, URL или путь.
export function GraphIcon({ name }: { name?: string }) {
  const glyph =
    name && Object.hasOwn(icons, name)
      ? icons[name as keyof typeof icons]
      : undefined;
  return glyph ? <Icon data={glyph} size={16} /> : null;
}
