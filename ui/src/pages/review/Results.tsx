import { useState } from 'react';
import { Icon, Label } from '@gravity-ui/uikit';
import { ChevronRight } from '@gravity-ui/icons';
import { Button, ErrorNotice, Dialog } from '../../components/ui';
import { counts, findingCount } from './helpers';
import { CopyPrompt, LinkLabel, ReviewMarkdown } from './Documents';
import { TourViewer } from './TourViewer';
import { ReviewExecution } from './Execution';
import type { Review, Tour, Finding } from './types';

// Список и экскурсии используют одну сохранённую презентацию. Пока агент её
// готовит, сырые замечания не выдаются за законченный доказательный результат.
export function ReviewResults({ review }: { review: Review }) {
  const [tour, setTour] = useState<Tour>();
  const [detail, setDetail] = useState<Finding>();
  const { blocking, other } = counts(review);
  const stage = review.Stages?.find((s) => s.ID === 'presentation');
  const prepared = !!review.Presentation.Overview?.Steps?.length;
  if (
    stage?.State !== 'succeeded' &&
    (!prepared || ['running', 'pending'].includes(stage?.State || 'pending'))
  )
    return <ReviewExecution review={review} stageID="presentation" />;
  return (
    <section className="cr-results">
      {!!review.Presentation.Overview?.Steps?.length && (
        <button
          className="cr-overview"
          onClick={() => setTour(review.Presentation.Overview)}
        >
          <strong>Обзор изменений</strong>
          <Icon data={ChevronRight} size={24} />
        </button>
      )}
      <h2 className={blocking ? 'cr-negative' : 'cr-positive'}>
        {blocking
          ? `Request changes · ${findingCount(blocking, true)}`
          : 'Approve'}
        {other ? ` · ${findingCount(other, false)}` : ''}
      </h2>
      {review.Presentation.Summary && (
        <ReviewMarkdown text={review.Presentation.Summary} />
      )}
      {(review.Findings || []).map((f) => {
        const related = (review.Context.Tasks || []).filter((t) =>
          f.Anchors?.some((a) => a.TaskID === t.ID),
        );
        const route = review.Presentation.FindingTours?.find(
          (t) => t.ID === f.TourID || t.FindingID === f.ID,
        );
        return (
          <article key={f.ID} className="cr-finding">
            <div className="cr-card-title">
              <Label
                theme={['P0', 'P1'].includes(f.Priority) ? 'danger' : 'normal'}
              >
                {f.Priority}
              </Label>
              <h3>{f.Title}</h3>
              <div className="cr-related-labels">
                {related.map((t) => (
                  <LinkLabel key={t.ID} label={t.ID} url={t.URL} />
                ))}
              </div>
            </div>
            <ReviewMarkdown text={f.Explanation} />
            <div className="cr-actions end">
              <CopyPrompt id={review.ID} path={f.FixPromptPath} />
              <Button onClick={() => (route ? setTour(route) : setDetail(f))}>
                Разобрать замечание
                <Icon data={ChevronRight} size={16} />
              </Button>
            </div>
          </article>
        );
      })}
      {(review.Publications || []).length > 0 && (
        <section className="cr-publications">
          <h3>Публикация</h3>
          {review.Publications!.map((p, i) => (
            <div key={`${p.FindingID}-${i}`}>
              {p.URL ? (
                <LinkLabel
                  label={
                    review.Findings?.find((f) => f.ID === p.FindingID)?.Title ||
                    'Комментарий ревью'
                  }
                  url={p.URL}
                />
              ) : (
                <span>{p.State}</span>
              )}
              <ErrorNotice error={p.Error} />
            </div>
          ))}
        </section>
      )}
      {detail && (
        <Dialog
          open
          onOpenChange={() => setDetail(undefined)}
          title={`${detail.Priority} · ${detail.Title}`}
        >
          <ReviewMarkdown text={detail.Explanation} />
          {detail.Reproduction && (
            <>
              <h3>Как воспроизводится</h3>
              <ReviewMarkdown text={detail.Reproduction} />
            </>
          )}
          {detail.Consequence && (
            <>
              <h3>Последствие</h3>
              <ReviewMarkdown text={detail.Consequence} />
            </>
          )}
          {detail.Anchors?.filter((a) => a.NoAnchorReason).map((a, i) => (
            <p className="cr-muted" key={i}>
              {a.NoAnchorReason}
            </p>
          ))}
        </Dialog>
      )}
      {tour && (
        <TourViewer
          review={review}
          tour={tour}
          close={() => setTour(undefined)}
        />
      )}
    </section>
  );
}
