import { useState } from 'react';
import {
  Button,
  ClipboardButton,
  Icon,
  Text,
  TextInput,
} from '@gravity-ui/uikit';
import { Persons } from '@gravity-ui/icons';
import { toaster } from '@gravity-ui/uikit/toaster-singleton';
import { Dialog } from './ui';
import { characterGallery } from './appearances';
import './appearance-picker.css';

// Независимый справочник внешности доступен даже без заказа. Копирование даёт
// стабильный ID из каталога, а просмотр не меняет выбранных сотрудникам образов.
export function CharacterGallery() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const search = query.trim().toLocaleLowerCase('ru');
  const characters = characterGallery.filter((item) =>
    `${item.id} ${item.name} ${item.profession}`
      .toLocaleLowerCase('ru')
      .includes(search),
  );
  return (
    <>
      <Button
        view="flat"
        title="Галерея персонажей"
        aria-label="Галерея персонажей"
        onClick={() => setOpen(true)}
      >
        <Icon data={Persons} />
      </Button>
      <Dialog
        open={open}
        onOpenChange={setOpen}
        title="Галерея персонажей"
        description="40 новых и 2 исходных образа. Скопируйте ID внешности для будущей настройки конфигов — это не @id сотрудника."
      >
        <div className="appearance-picker">
          <TextInput
            value={query}
            onUpdate={setQuery}
            placeholder="Имя, профессия или ID"
            controlProps={{ 'aria-label': 'Поиск персонажей' }}
            hasClear
          />
          <div
            className="appearance-grid character-gallery-grid"
            role="list"
            aria-label="Все персонажи"
          >
            {characters.map((item) => (
              <article
                key={item.id}
                className="character-gallery-card"
                role="listitem"
                aria-label={item.name}
              >
                <img
                  src={item.sprite}
                  width="1254"
                  height="1254"
                  alt={item.name}
                  loading="lazy"
                />
                <Text variant="subheader-1">{item.name}</Text>
                <Text variant="caption-2" color="secondary">
                  {item.profession}
                </Text>
                <div className="character-gallery-id">
                  <code>{item.id}</code>
                  <ClipboardButton
                    text={item.id}
                    view="flat"
                    size="s"
                    aria-label={`Скопировать ID ${item.id}`}
                    tooltipInitialText="Скопировать ID"
                    tooltipSuccessText="ID скопирован"
                    onCopy={(_, copied) => {
                      if (!copied)
                        toaster.add({
                          name: 'copy-appearance-id',
                          title: 'Не удалось скопировать ID',
                          theme: 'danger',
                          autoHiding: 2500,
                        });
                    }}
                  />
                </div>
              </article>
            ))}
          </div>
          {!characters.length && (
            <Text color="secondary">Персонажи не найдены</Text>
          )}
        </div>
      </Dialog>
    </>
  );
}
