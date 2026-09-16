import { Button, Icon, Select, Tooltip } from '@gravity-ui/uikit';
import { ArrowRotateLeft } from '@gravity-ui/icons';
import type { Dashboard } from '../types';

const statuses = [
  { value: 'active', content: 'Активные' },
  { value: 'working', content: 'В работе' },
  { value: 'failed', content: 'С ошибками' },
  { value: 'completed', content: 'Завершённые' },
  { value: 'all', content: 'Все' },
];
const periods: Record<string, string> = {
  '1h': '1 час',
  '2h': '2 часа',
  '4h': '4 часа',
  '8h': '8 часов',
  '12h': '12 часов',
  '24h': '24 часа',
  '2d': '2 дня',
  '5d': '5 дней',
  '7d': '7 дней',
  '14d': '14 дней',
  '30d': '30 дней',
  all: 'Всё время',
};

// Один выбор атомарно задаёт оба параметра API. Ошибки ищутся среди всех
// деревьев, иначе старый active-scope скрывал бы полностью завершённые ошибки.
export function statusParams(value: string): Record<string, string> {
  if (value === 'working' || value === 'failed')
    return { view: 'all', states: value };
  return { view: value === 'active' ? '' : value, states: '' };
}
export function filtersChanged(data: Dashboard): boolean {
  const f = data.Filter;
  return (
    f.Scope !== 'active' ||
    f.States !== 'all' ||
    f.Period !== '24h' ||
    Boolean(f.Query || f.RootID) ||
    data.Pagination.Current > 1
  );
}
// Старые закладки с сочетанием active+failed остаются честно подписанными:
// интерфейс не выдаёт ограниченную выборку за все ошибки и не переписывает URL.
export function DashboardFilters({
  data,
  onChange,
  onReset,
}: {
  data: Dashboard;
  onChange: (values: Record<string, string>) => void;
  onReset: () => void;
}) {
  const f = data.Filter;
  const state =
    f.States === 'working' || f.States === 'failed' ? f.States : undefined;
  const legacy = state && f.Scope !== 'all';
  const value = legacy ? 'legacy' : state || f.Scope;
  const options = legacy
    ? [
        ...statuses,
        {
          value: 'legacy',
          content: `${state === 'failed' ? 'С ошибками' : 'В работе'} · ${f.Scope === 'completed' ? 'завершённые' : 'активные'}`,
        },
      ]
    : statuses;
  return (
    <>
      <Select
        aria-label="Статус workflow"

        value={[value]}
        options={options}
        onUpdate={([next]) => {
          if (next && next !== 'legacy') onChange(statusParams(next));
        }}
      />
      <Select
        aria-label="Период"

        value={[f.Period]}
        options={f.Periods.map((p) => ({ value: p.Value, content: p.Label }))}
        renderSelectedOption={(option) => (
          <>{periods[option.value] || option.content}</>
        )}
        onUpdate={([period]) => {
          if (period) onChange({ period });
        }}
      />
      {filtersChanged(data) && (
        <Tooltip content="Сбросить фильтры, поиск и выбранную папку">
          <Button view="flat" aria-label="Сбросить фильтры" onClick={onReset}>
            <Icon data={ArrowRotateLeft} />
          </Button>
        </Tooltip>
      )}
    </>
  );
}
