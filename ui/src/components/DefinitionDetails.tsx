import type { Graph, GraphNode } from '../types';
import { MarkdownDocument } from './MarkdownDocument';
import { Choice } from './ui';

// Панель определения не показывает статусы, visits и кнопки runtime.
// Выбор через список доступен и без мыши; схема использует тот же selected step.
export function DefinitionDetails({
  graph,
  selected,
  onSelect,
}: {
  graph: Graph;
  selected?: GraphNode;
  onSelect: (step: string) => void;
}) {
  const details = selected?.Definition;
  const character = details?.Character;
  const inherited =
    'Из конфигурации Codex при запуске; значение пока неизвестно';
  return (
    <>
      <div className="graph-heading">
        <h1>{graph.Name}</h1>
      </div>
      <div className="cube-details-content">
        <p className="muted">
          Определение workflow v{graph.Version}. Выполнение не запущено.
        </p>
        <Choice
          aria-label="Шаг workflow"
          value={selected?.ID || ''}
          onUpdate={onSelect}
          options={(graph.Nodes || []).map((node) => ({
            value: node.ID,
            content: `${node.Definition?.Character?.name || node.ID}${node.Definition?.Start ? ' · Старт' : ''}`,
          }))}
        />
        <h2>{character?.name || selected?.ID}</h2>
        <p>
          {selected?.ID}
          {details?.Start ? ' · Стартовый шаг' : ''}
        </p>
        <h3>Переходы и остановки</h3>
        <ul>
          {(selected?.Routes || []).map((route) => (
            <li key={route}>{route}</li>
          ))}
        </ul>
        {(graph.Edges || [])
          .filter((edge) => edge.To === selected?.ID && !edge.Label)
          .map((edge) => (
            <p key={edge.From}>Зависит от: {edge.From}</p>
          ))}
        {graph.Version === 1 && (
          <p className="muted">
            Шаг ждёт успешного завершения зависимостей. Workflow завершается
            после выполнения всех шагов; ошибка шага останавливает зависимые
            шаги.
          </p>
        )}
        {graph.Version === 2 && (
          <p className="muted">
            after ждёт завершения источника; именованный переход выбирает агент.
            finish завершает workflow, maxVisits ограничивает посещения, onLimit
            задаёт итог при превышении лимита. Без finish workflow завершается
            после исчерпания работы.
          </p>
        )}
        <h3>Настройки запуска</h3>
        <dl className="visit-facts">
          <dt>Модель</dt>
          <dd>
            {details?.Model
              ? `${details.Model} · ${details.ModelSource === 'step.model' ? 'задана в шаге' : 'унаследована из workflow.model'}`
              : inherited}
          </dd>
          <dt>Усилие рассуждения</dt>
          <dd>
            {details?.Effort ? `${details.Effort} · задано в шаге` : inherited}
          </dd>
          <dt>Скорость</dt>
          <dd>
            {details?.Speed ? `${details.Speed} · задана в шаге` : inherited}
          </dd>
        </dl>
        {character ? (
          <>
            <h3>Личность · {details?.CharacterID}</h3>
            <h4>Предыстория</h4>
            <MarkdownDocument
              text={character.history}
              label="Предыстория личности"
            />
            <h4>Принципы и границы</h4>
            <MarkdownDocument
              text={character.instructions}
              label="Инструкции личности"
            />
          </>
        ) : (
          <p className="muted">Личность не задана.</p>
        )}
        <h3>Инструкция шага</h3>
        <MarkdownDocument
          text={selected?.Prompt || ''}
          label="Эффективная инструкция шага"
        />
        <p className="muted">
          Markdown и шаблоны раскрыты. Задача, память и сведения о посещении
          добавляются только при исполнении.
        </p>
      </div>
    </>
  );
}
