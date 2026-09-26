import { useState } from 'react';
import { Icon, Label, TabProvider, TabList, Tab } from '@gravity-ui/uikit';
import { Code, File as FileIcon } from '@gravity-ui/icons';
import { Button, Dialog } from '../../components/ui';
import { artifactURL } from './helpers';
import { LinkLabel, SavedText, TextDialog, DiffDialog } from './Documents';
import type { Review, Task, Design, File, Command } from './types';

// Сохранённые материалы остаются доступны при работающей проверке, без чтения
// текущего checkout вместо исторического снимка.
export function ReviewContext({ review }: { review: Review }) {
  const [tab, setTab] = useState('tasks');
  const collecting =
    ['pending', 'running'].includes(review.State) &&
    ['pending', 'running'].includes(
      review.Stages?.find((s) => s.ID === 'context')?.State || 'pending',
    );
  const ctx = review.Context,
    stats = ctx.Stats;
  const [file, setFile] = useState<File>(),
    [command, setCommand] = useState<Command>(),
    [diff, setDiff] = useState(false);
  const empty = (list: unknown[] | null) => !(list || []).length;
  return (
    <section className="cr-context">
      <TabProvider value={tab} onUpdate={setTab}>
        <TabList>
          <Tab value="tasks">
            Задачи{ctx.Tasks?.length ? ` ${ctx.Tasks.length}` : ''}
          </Tab>
          <Tab value="design">
            Дизайн{ctx.Designs?.length ? ` ${ctx.Designs.length}` : ''}
          </Tab>
          <Tab value="changes">
            Изменения{' '}
            <span className="cr-tab-stats">
              {(stats.AddedLines > 0 || stats.DeletedLines > 0) && (
                <span className="cr-stat-chip" title="Строки кода">
                  <Icon data={Code} size={14} />
                  {stats.AddedLines > 0 && (
                    <b className="cr-positive">+{stats.AddedLines}</b>
                  )}
                  {stats.DeletedLines > 0 && (
                    <b className="cr-negative">−{stats.DeletedLines}</b>
                  )}
                </span>
              )}
              {stats.AddedFiles + stats.DeletedFiles + stats.ModifiedFiles >
                0 && (
                <span
                  className="cr-stat-chip"
                  title="Добавленные, удалённые и изменённые файлы"
                >
                  <Icon data={FileIcon} size={14} />
                  {stats.AddedFiles > 0 && (
                    <b className="cr-positive">+{stats.AddedFiles}</b>
                  )}
                  {stats.DeletedFiles > 0 && (
                    <b className="cr-negative">−{stats.DeletedFiles}</b>
                  )}
                  {stats.ModifiedFiles > 0 && (
                    <span>~{stats.ModifiedFiles}</span>
                  )}
                </span>
              )}
            </span>
          </Tab>
          <Tab value="project">Контекст проекта</Tab>
        </TabList>
      </TabProvider>
      {tab === 'tasks' && (
        <div className="cr-materials">
          {(ctx.Tasks || []).map((task) => (
            <TaskCard key={task.ID} id={review.ID} task={task} />
          ))}
          {empty(ctx.Tasks) && <ContextEmpty loading={collecting} />}
        </div>
      )}
      {tab === 'design' && (
        <div className="cr-design-grid">
          {(ctx.Designs || []).map((design) => (
            <DesignImage key={design.ID} id={review.ID} design={design} />
          ))}
          {empty(ctx.Designs) && <ContextEmpty loading={collecting} />}
        </div>
      )}
      {tab === 'changes' && (
        <div className="cr-materials">
          {ctx.DiffPath && (
            <Button onClick={() => setDiff(true)}>Полный diff</Button>
          )}
          {(ctx.Changes || []).map((f) => (
            <Button
              className="cr-file-row"
              key={f.ID}
              onClick={() => setFile(f)}
            >
              <Icon data={FileIcon} />
              {f.Path}
              <span className="cr-positive">
                {f.AddedLines ? `+${f.AddedLines}` : ''}
              </span>
              <span className="cr-negative">
                {f.DeletedLines ? `−${f.DeletedLines}` : ''}
              </span>
            </Button>
          ))}
          {empty(ctx.Changes) && !ctx.DiffPath && (
            <ContextEmpty loading={collecting} />
          )}
        </div>
      )}
      {tab === 'project' && (
        <div className="cr-materials">
          {(ctx.ProjectFiles || []).map((f) => (
            <Button
              className="cr-file-row"
              key={f.ID}
              onClick={() => setFile(f)}
            >
              <Icon data={FileIcon} />
              {f.Path}
              {f.StartLine > 0 && (
                <span className="cr-muted">
                  {f.StartLine}–{f.EndLine}
                </span>
              )}
            </Button>
          ))}
          {(ctx.Commands || []).map((c) => (
            <Button
              className="cr-file-row"
              key={c.ID}
              onClick={() => setCommand(c)}
            >
              <Icon data={Code} />
              {c.Command}
            </Button>
          ))}
          {empty(ctx.ProjectFiles) && empty(ctx.Commands) && (
            <ContextEmpty loading={collecting} />
          )}
        </div>
      )}
      {file && (
        <TextDialog
          id={review.ID}
          path={file.SnapshotPath}
          title={file.Path}
          close={() => setFile(undefined)}
          markdown={false}
        />
      )}
      {diff && (
        <DiffDialog
          id={review.ID}
          path={ctx.DiffPath}
          close={() => setDiff(false)}
        />
      )}
      {command && (
        <Dialog
          open
          onOpenChange={() => setCommand(undefined)}
          title="Выполненная команда"
        >
          <pre className="cr-code-raw">{command.Command}</pre>
          <p className="cr-muted">
            {command.CWD} · Код завершения: {command.ExitCode ?? '—'}
          </p>
          {command.ScriptPath && (
            <SavedText
              id={review.ID}
              path={command.ScriptPath}
              markdown={false}
            />
          )}
          <SavedText
            id={review.ID}
            path={command.OutputPath}
            markdown={false}
          />
        </Dialog>
      )}
    </section>
  );
}
function ContextEmpty({ loading }: { loading: boolean }) {
  return loading ? (
    <div className="cr-skeleton-group" aria-label="Агент собирает материалы">
      <div className="cr-skeleton" />
      <div className="cr-skeleton" />
      <div className="cr-skeleton" />
    </div>
  ) : (
    <p className="cr-muted">Материалов нет</p>
  );
}
function TaskCard({ id, task }: { id: string; task: Task }) {
  const [mode, setMode] = useState('');
  return (
    <article className="cr-task-card">
      <div className="cr-card-title">
        <Label>{task.ID}</Label>
        <h2>{task.Title}</h2>
      </div>
      <SavedText id={id} path={task.BodyPath} preview />
      <div className="cr-actions">
        <Button onClick={() => setMode('body')}>Полный текст</Button>
        <Button onClick={() => setMode('comments')}>
          Комментарии · {task.Comments?.length || 0}
        </Button>
      </div>
      {mode && (
        <Dialog
          open
          onOpenChange={() => setMode('')}
          title={mode === 'body' ? task.Title : `Комментарии · ${task.ID}`}
        >
          {mode === 'body' ? (
            <>
              <LinkLabel label={task.ID} url={task.URL} />
              <SavedText id={id} path={task.BodyPath} />
            </>
          ) : (task.Comments || []).length ? (
            task.Comments!.map((c) => (
              <article key={c.ID} className="cr-comment">
                <strong>{c.Author}</strong>
                <span className="cr-muted"> {c.CreatedAt}</span>
                <SavedText id={id} path={c.BodyPath} />
              </article>
            ))
          ) : (
            <p className="cr-muted">Комментариев нет</p>
          )}
        </Dialog>
      )}
    </article>
  );
}
export function DesignImage({ id, design }: { id: string; design: Design }) {
  const [open, setOpen] = useState(false);
  const url = artifactURL(id, design.Path);
  return (
    <section className="cr-design">
      <h3>{design.Title}</h3>
      <button
        type="button"
        className="cr-image-button"
        onClick={() => setOpen(true)}
        aria-label={`Увеличить: ${design.Title}`}
      >
        <img src={url} alt={design.Title} />
      </button>
      <Dialog open={open} onOpenChange={setOpen} title={design.Title}>
        <img className="cr-expanded-image" src={url} alt={design.Title} />
        <LinkLabel label="Источник дизайна" url={design.SourceURL} />
      </Dialog>
    </section>
  );
}
