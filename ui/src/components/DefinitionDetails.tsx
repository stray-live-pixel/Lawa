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
  const inherited = 'Из Codex · пока неизвестно';
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
        <details className="definition-disclosure">
          <summary>Правила переходов</summary>
          {graph.Version === 1 && (
            <p className="muted">
              Шаг ждёт успешного завершения зависимостей. Workflow завершается
              после выполнения всех шагов; ошибка шага останавливает зависимые
              шаги.
            </p>
          )}
          {graph.Version === 2 && (
            <p className="muted">
              after ждёт завершения источника; именованный переход выбирает
              агент. finish завершает workflow, maxVisits ограничивает
              посещения, onLimit задаёт итог при превышении лимита. Без finish
              workflow завершается после исчерпания работы.
            </p>
          )}
        </details>
        <MarkdownDocument
          key={selected?.ID}
          text={selected?.Prompt || ''}
          label="Инструкция шага"
          reader
        />
        <details className="definition-disclosure">
          <summary>Настройки и сведения о шаге</summary>
          <p>
            Шаг: {selected?.ID}
            {details?.Start ? ' · Стартовый' : ''}
          </p>
          <p className="muted">
            Незаданные настройки берутся из конфигурации Codex при запуске; их
            значения пока неизвестны.
          </p>

          <p className="muted">
            В инструкции раскрыты Markdown-файлы и шаблоны. Задача, память и
            сведения о посещении добавляются при исполнении.
          </p>
          <dl className="visit-facts">
            <dt>Модель</dt>
            <dd>
              {details?.Model
                ? `${details.Model} · ${details.ModelSource === 'step.model' ? 'задана в шаге' : 'унаследована из workflow.model'}`
                : inherited}
            </dd>
            <dt>Усилие рассуждения</dt>
            <dd>
              {details?.Effort
                ? `${details.Effort} · задано в шаге`
                : inherited}
            </dd>
            <dt>Скорость</dt>
            <dd>
              {details?.Speed ? `${details.Speed} · задана в шаге` : inherited}
            </dd>
          </dl>
        </details>
        <details className="definition-disclosure">
          <summary>
            Личность{character ? ` · ${character.name}` : ' не задана'}
          </summary>
          {character ? (
            <>
              <h3>Личность · {details?.CharacterID}</h3>
              <h4>Предыстория</h4>
              <MarkdownDocument
                text={character.history}
                label="Предыстория личности"
                reader
              />
              <h4>Принципы и границы</h4>
              <MarkdownDocument
                text={character.instructions}
                label="Инструкции личности"
                reader
              />
            </>
          ) : (
            <p className="muted">Личность не задана.</p>
          )}
        </details>
      </div>
    </>
  );
}
