import { useState } from 'react';
import { Button, Icon, Text, TextInput } from '@gravity-ui/uikit';
import { Palette, Shuffle, ArrowRotateLeft } from '@gravity-ui/icons';
import { Choice, Dialog } from './ui';
import { appearances, randomAppearance, useAppearances } from './appearances';
import './appearance-picker.css';

// Изменение применяется сразу к выбранному сотруднику. Имена образов — подсказки
// для выбора, а рабочие имена, @id и задачи сотрудников остаются прежними.
export function AppearancePicker({
  members,
}: {
  members: Record<string, { name: string }>;
}) {
  const [open, setOpen] = useState(false);
  const [actor, setActor] = useState('boss');
  const [query, setQuery] = useState('');
  const { choices, choose } = useAppearances();
  const activeActor = Object.hasOwn(members, actor)
    ? actor
    : Object.keys(members)[0];
  const current = choices[activeActor];
  const search = query.trim().toLocaleLowerCase('ru');
  const filtered = appearances.filter((item) =>
    `${item.name} ${item.profession}`.toLocaleLowerCase('ru').includes(search),
  );
  return (
    <>
      <Button
        view="flat"
        aria-label="Внешность сотрудников"
        title="Внешность сотрудников"
        onClick={() => setOpen(true)}
      >
        <Icon data={Palette} />
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Внешность сотрудников"
        description="Образ меняет персонажа в офисе и аватарку в чате. Выбор сохраняется в этом браузере для текущего заказа."
      >
        <div className="appearance-picker">
          <div className="appearance-controls">
            <Choice
              aria-label="Сотрудник"
              value={activeActor}
              onUpdate={setActor}
              options={Object.entries(members).map(([value, member]) => ({
                value,
                content: `${member.name} · @${value}`,
              }))}
            />
            <Button
              view="outlined"
              onClick={() => choose(activeActor, randomAppearance(current))}
            >
              <Icon data={Shuffle} /> Случайный образ
            </Button>
            <Button
              view="flat"
              disabled={!current}
              onClick={() => choose(activeActor)}
              title="Вернуть исходный образ"
              aria-label="Вернуть исходный образ"
            >
              <Icon data={ArrowRotateLeft} />
            </Button>
          </div>
          <TextInput
            value={query}
            onUpdate={setQuery}
            placeholder="Найти образ или профессию"
            controlProps={{ 'aria-label': 'Поиск внешности' }}
            hasClear
          />
          <div className="appearance-grid" aria-label="Коллекция внешности">
            {filtered.map((item) => (
              <Button
                key={item.id}
                view={current === item.id ? 'normal' : 'outlined'}
                selected={current === item.id}
                className="appearance-card"
                aria-label={`Выбрать образ: ${item.name}`}
                aria-pressed={current === item.id}
                onClick={() => choose(activeActor, item.id)}
              >
                <span className="appearance-card-content">
                  <img
                    src={item.sprite}
                    width="1024"
                    height="1024"
                    alt=""
                    loading="lazy"
                  />
                  <Text variant="body-1">{item.name}</Text>
                  <Text variant="caption-2" color="secondary">
                    {item.profession}
                  </Text>
                </span>
              </Button>
            ))}
          </div>
          {!filtered.length && <Text color="secondary">Таких образов нет</Text>}
        </div>
      </Dialog>
    </>
  );
}
