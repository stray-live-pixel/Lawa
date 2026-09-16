import { useState } from 'react';
import { TextArea } from '@gravity-ui/uikit';
import { usePoll } from '../hooks/api';
import type { Graph } from '../types';
import { Choice, ErrorNotice, Button } from './ui';
import { MarkdownDocument } from './MarkdownDocument';

interface Source {
  JSON: unknown;
  Documents: { Name: string; Content: string }[];
  Note: string;
}
// Preview не выдаёт topology DTO за исполняемый JSON: показывает явно обозначенный
// демонстрационный документ. В live читается сохранённый snapshot по runID.
export function WorkflowSource({
  runID,
  preview,
}: {
  runID: string;
  preview?: Graph;
}) {
  const { data, error } = usePoll<Source>(
    preview ? null : `/api/source/${encodeURIComponent(runID)}`,
    0,
  );
  const [selected, setSelected] = useState('json');
  const [raw, setRaw] = useState(false);
  const source: Source | undefined = preview
    ? {
        JSON: {
          id: preview.Name,
          demo: true,
          nodes: preview.Nodes,
          edges: preview.Edges,
        },
        Documents: (preview.Nodes || []).map((node) => ({
          Name: `Инструкция · ${node.ID}`,
          Content:
            'Демонстрационные данные. Исходный Markdown рабочего workflow не включён в preview.',
        })),
        Note: 'Демонстрационная схема, не JSON для запуска. Реальные исходники доступны у сохранённого запуска.',
      }
    : data;
  if (!source)
    return (
      <div className="loading">
        <ErrorNotice error={error} />
        {!error && 'Загрузка исходников…'}
      </div>
    );
  const document = source.Documents[Number(selected)];
  return (
    <section className="workflow-source" aria-label="Исходники workflow">
      <ErrorNotice error={error} />
      <p className="muted">{source.Note}</p>
      <div className="actions">
        <Choice
          aria-label="Файл workflow"
          value={selected}
          onUpdate={setSelected}
          options={[
            { value: 'json', content: 'workflow.json' },
            ...source.Documents.map((doc, index) => ({
              value: String(index),
              content: doc.Name,
            })),
          ]}
        />
        {document && (
          <Button view="flat" onClick={() => setRaw(!raw)}>
            {raw ? 'Показать Markdown' : 'Исходный текст'}
          </Button>
        )}
      </div>
      {!document ? (
        <TextArea
          controlProps={{ 'aria-label': 'JSON workflow' }}
          value={JSON.stringify(source.JSON, null, 2)}
          readOnly
          rows={28}
        />
      ) : raw ? (
        <TextArea
          controlProps={{ 'aria-label': 'Исходный Markdown' }}
          value={document.Content}
          readOnly
          rows={28}
        />
      ) : (
        <MarkdownDocument text={document.Content} label={document.Name} />
      )}
    </section>
  );
}
