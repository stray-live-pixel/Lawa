import { useEffect, useState, useRef } from 'react';
import { Icon, Modal } from '@gravity-ui/uikit';
import {
  ChevronLeft,
  ChevronRight,
  TriangleExclamation,
  Xmark,
} from '@gravity-ui/icons';
import { Button, ErrorNotice, Dialog } from '../../components/ui';
import { usePoll } from '../../hooks/api';
import { artifactURL, mutate } from './helpers';
import { ReviewMarkdown, LinkLabel, TextDialog, SavedText } from './Documents';
import { DesignImage } from './Context';
import type { Anchor, File, Review, Tour, TourStep, ViewState } from './types';

// Экскурсия читает только зафиксированные артефакты. Diff-семантика и место
// ошибки независимы: добавленный ошибочный код остаётся зелёным.
export function TourViewer({
  review,
  tour,
  close,
}: {
  review: Review;
  tour: Tour;
  close: () => void;
}) {
  const [index, setIndex] = useState(0);
  const viewedRevision = useRef(review.Revision);
  const [restored, setRestored] = useState(false);
  const savedView = usePoll<ViewState>(
    `/api/reviews/${encodeURIComponent(review.ID)}/view`,
    0,
  );
  const steps = tour.Steps || [],
    step = steps[index];
  const [file, setFile] = useState<File>();
  const [commentTask, setCommentTask] = useState<string>();
  useEffect(() => {
    setIndex(0);
    setRestored(false);
  }, [tour.ID]);
  useEffect(() => {
    if (restored || (!savedView.data && !savedView.error)) return;
    const savedIndex =
      savedView.data?.SelectedTour === tour.ID
        ? steps.findIndex((s) => s.ID === savedView.data?.SelectedStep)
        : -1;
    setIndex(Math.max(0, savedIndex));
    setRestored(true);
  }, [savedView.data, savedView.error, restored, tour.ID, steps]);
  useEffect(() => {
    if (step && restored)
      void mutate(`/api/reviews/${encodeURIComponent(review.ID)}/view`, {
        SeenRevision: viewedRevision.current,
        SelectedStage: 'presentation',
        SelectedTour: tour.ID,
        SelectedStep: step.ID,
      }).catch(() => {});
  }, [step?.ID, tour.ID, review.ID, review.Revision, restored]);
  const allFiles = [
    ...(review.Context.Changes || []),
    ...(review.Context.ProjectFiles || []),
    ...(review.EvidenceFiles || []),
  ];
  const designs = [
    ...(review.Context.Designs || []),
    ...(review.EvidenceDesigns || []),
  ];
  const anchors = step?.Anchors || [];
  const tasks = (review.Context.Tasks || []).filter((t) =>
    anchors.some((a) => a.TaskID === t.ID),
  );
  const images = designs.filter((d) =>
    anchors.some((a) => a.DesignID === d.ID),
  );
  return (
    <Modal
      open
      onOpenChange={close}
      contentClassName="cr-tour-modal"
      aria-labelledby="cr-tour-title"
    >
      <div className="cr-tour-header">
        <h2 id="cr-tour-title">{tour.Title}</h2>
        <Button
          view="flat"
          aria-label="Закрыть обзор"
          title="Закрыть"
          onClick={close}
        >
          <Icon data={Xmark} />
        </Button>
      </div>
      {step ? (
        <div
          className={`cr-tour-body ${tasks.length || images.length ? '' : 'no-evidence'}`}
        >
          <nav className="cr-tour-steps" aria-label="Шаги обзора">
            <h3>{tour.FindingID ? 'Путь проблемы' : 'Путь изменений'}</h3>
            {steps.map((s, i) => (
              <button
                className={i === index ? 'selected' : ''}
                key={s.ID}
                onClick={() => setIndex(i)}
                aria-current={i === index ? 'step' : undefined}
              >
                {i + 1} {s.Title}
              </button>
            ))}
          </nav>
          <main className="cr-tour-code">
            <h3>{step.Title}</h3>
            {anchors
              .filter((a) => a.FileID)
              .map((a, i) => {
                const source = allFiles.find((f) => f.ID === a.FileID);
                return source ? (
                  <CodeExcerpt
                    key={`${a.FileID}-${i}`}
                    reviewID={review.ID}
                    file={source}
                    anchor={a}
                    step={step}
                    open={() => setFile(source)}
                  />
                ) : (
                  <p key={i} role="alert">
                    Снимок кода не найден: {a.FileID}
                  </p>
                );
              })}
            <div
              className={`cr-annotation ${step.ErrorLocation ? 'error' : ''}`}
            >
              {step.ErrorLocation && (
                <Icon data={TriangleExclamation} size={20} />
              )}
              <ReviewMarkdown text={step.Body} />
            </div>
            <div className="cr-actions">
              {allFiles
                .filter((f) => anchors.some((a) => a.FileID === f.ID))
                .map((f) => (
                  <Button key={f.ID} onClick={() => setFile(f)}>
                    {anchors.filter((a) => a.FileID).length > 1
                      ? f.Path
                      : 'Открыть сохранённый код'}
                  </Button>
                ))}
            </div>
            {anchors
              .filter((a) => a.NoAnchorReason)
              .map((a, i) => (
                <p className="cr-muted" key={i}>
                  {a.NoAnchorReason}
                </p>
              ))}
          </main>
          <aside className="cr-tour-evidence">
            {tasks.length > 0 && <h3>Основание</h3>}
            {tasks.map((t) => (
              <section className="cr-evidence" key={t.ID}>
                <LinkLabel label={t.ID} url={t.URL} />
                <h4>{t.Title}</h4>
                <EvidenceTask id={review.ID} path={t.BodyPath} />
                {!!t.Comments?.length && (
                  <Button size="s" onClick={() => setCommentTask(t.ID)}>
                    Комментарии · {t.Comments.length}
                  </Button>
                )}
              </section>
            ))}
            {images.map((d) => (
              <DesignImage key={d.ID} id={review.ID} design={d} />
            ))}
          </aside>
        </div>
      ) : (
        <p className="cr-muted">Шаги обзора ещё не подготовлены</p>
      )}
      <footer className="cr-tour-footer">
        <Button disabled={index === 0} onClick={() => setIndex((i) => i - 1)}>
          <Icon data={ChevronLeft} />
          Назад
        </Button>
        <span className="cr-muted">
          {steps.length ? index + 1 : 0} / {steps.length}
        </span>
        <Button
          onClick={() =>
            index + 1 < steps.length ? setIndex((i) => i + 1) : close()
          }
        >
          {index + 1 < steps.length ? 'Следующий шаг' : 'Завершить обзор'}
          <Icon data={ChevronRight} />
        </Button>
      </footer>
      {commentTask && (
        <Dialog
          open
          onOpenChange={() => setCommentTask(undefined)}
          title={`Комментарии · ${commentTask}`}
        >
          {review.Context.Tasks?.find(
            (t) => t.ID === commentTask,
          )?.Comments?.map((c) => (
            <article className="cr-comment" key={c.ID}>
              <strong>{c.Author}</strong>
              <span className="cr-muted"> {c.CreatedAt}</span>
              <SavedText id={review.ID} path={c.BodyPath} />
            </article>
          ))}
        </Dialog>
      )}
      {file && (
        <TextDialog
          id={review.ID}
          path={file.SnapshotPath}
          title={`${file.Path} · сохранённый снимок${file.StartLine > 0 && file.EndLine > 0 ? ` · строки ${file.StartLine}–${file.EndLine}` : ''}`}
          close={() => setFile(undefined)}
          markdown={false}
        />
      )}
    </Modal>
  );
}
function EvidenceTask({ id, path }: { id: string; path: string }) {
  const { data, error } = usePoll<string>(
    path ? artifactURL(id, path) : null,
    0,
    'text',
  );
  const [full, setFull] = useState(false);
  return (
    <>
      <ErrorNotice error={error} />
      <div className={full ? '' : 'cr-text-preview'}>
        <ReviewMarkdown text={data || ''} />
      </div>
      {data && (
        <Button size="s" onClick={() => setFull(!full)}>
          {full ? 'Свернуть' : 'Полное требование'}
        </Button>
      )}
    </>
  );
}
function CodeExcerpt({
  reviewID,
  file,
  anchor,
  step,
  open,
}: {
  reviewID: string;
  file: File;
  anchor: Anchor;
  step: TourStep;
  open: () => void;
}) {
  const { data, error } = usePoll<string>(
    artifactURL(reviewID, file.SnapshotPath),
    0,
    'text',
  );
  const start = file.StartLine || 1,
    from = Math.max(start, (anchor.StartLine || start) - 3),
    to = anchor.EndLine || anchor.StartLine || file.EndLine || from + 20;
  const lines = (data || '')
    .split('\n')
    .map((text, i) => ({ text, line: start + i }))
    .filter((l) => l.line >= from && l.line <= to + 3);
  const baseColor = ['added', 'add', 'new'].includes(file.Change)
    ? 'added'
    : ['deleted', 'delete', 'removed'].includes(file.Change)
      ? 'deleted'
      : 'context';
  return (
    <section className="cr-excerpt">
      <p className="cr-muted">
        {file.Path} · {anchor.StartLine || start}–{to}
      </p>
      <ErrorNotice error={error} />
      {data === undefined ? (
        <div className="cr-skeleton" />
      ) : (
        <div className="cr-code-block">
          {lines.map((l) => {
            const color =
              file.LineChanges?.find(
                (c) => l.line >= c.StartLine && l.line <= c.EndLine,
              )?.Kind || baseColor;
            const selected =
              l.line >= (anchor.StartLine || start) && l.line <= to;
            return (
              <div key={l.line}>
                {selected &&
                  l.line === (anchor.StartLine || start) &&
                  step.ErrorLocation && (
                    <strong className="cr-error-location">Место ошибки</strong>
                  )}
                <div className={`cr-code-line ${selected ? color : ''}`}>
                  <span className="cr-line-no">{l.line}</span>
                  <span className="cr-line-sign">
                    {selected
                      ? color === 'added'
                        ? '+'
                        : color === 'deleted'
                          ? '−'
                          : ''
                      : ''}
                  </span>
                  <code>{l.text || ' '}</code>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </section>
  );
}
