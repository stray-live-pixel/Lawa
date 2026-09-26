// Только тестовые данные: не импортируются production-маршрутом.
import { defaultConfig } from './helpers';
import type { Review } from './types';
export function fixture(): Review {
  return {
    Version: 1,
    Kind: 'review',
    ID: 'review-fixture',
    CWD: '/workspace/project',
    Prompt: 'Проверить форму входа',
    Title: 'Проверка формы входа',
    CreatedAt: '2026-09-26T01:30:00Z',
    UpdatedAt: '2026-09-26T01:34:00Z',
    Revision: 5,
    State: 'succeeded',
    CurrentStage: 'presentation',
    Config: {
      ...defaultConfig,
      Prices: {
        'gpt-6-sol': {
          Input: 2,
          CachedInput: 0.2,
          Output: 10,
          Source: 'https://example.test/pricing',
          AsOf: '2026-09-26',
        },
      },
    },
    Stages: [
      {
        ID: 'context',
        State: 'succeeded',
        Attempts: [
          {
            Number: 1,
            State: 'succeeded',
            StartedAt: '2026-09-26T01:30:00Z',
            FinishedAt: '2026-09-26T01:31:00Z',
            ThreadID: 't',
            TurnID: '1',
            PromptPath: 'artifacts/prompt.md',
            OutputPath: '',
            Error: '',
            Usage: {
              InputTokens: 1000,
              CachedInputTokens: 200,
              OutputTokens: 100,
              CostUSD: 0.00264,
              Complete: true,
            },
          },
        ],
      },
      { ID: 'review', State: 'succeeded', Attempts: [] },
      { ID: 'presentation', State: 'succeeded', Attempts: [] },
    ],
    Context: {
      Tasks: [
        {
          ID: 'TASK-42',
          Title: 'Форма входа',
          URL: 'https://example.test/tasks/42',
          BodyPath: 'artifacts/task.md',
          Comments: [],
        },
      ],
      Designs: [],
      Changes: [
        {
          ID: 'file-1',
          Path: 'src/LoginForm.tsx',
          SnapshotPath: 'artifacts/code.txt',
          StartLine: 40,
          EndLine: 46,
          Change: 'modified',
          AddedLines: 4,
          DeletedLines: 0,
          Revision: 'abc',
          LineChanges: [{ StartLine: 42, EndLine: 45, Kind: 'added' }],
        },
      ],
      ProjectFiles: [],
      Commands: [],
      DiffPath: 'artifacts/diff.txt',
      Links: [],
      Stats: {
        AddedLines: 4,
        DeletedLines: 0,
        AddedFiles: 0,
        DeletedFiles: 0,
        ModifiedFiles: 1,
      },
      FrozenAt: '2026-09-26T01:31:00Z',
    },
    Findings: [
      {
        ID: 'finding-1',
        Priority: 'P1',
        Title: 'Повторная отправка создаёт два запроса',
        Explanation: 'Кнопка остаётся доступной во время запроса.',
        Reproduction: 'Нажмите Войти дважды.',
        Consequence: 'Два запроса.',
        Anchors: [
          {
            FileID: 'file-1',
            StartLine: 42,
            EndLine: 45,
            TaskID: 'TASK-42',
            DesignID: '',
            NoAnchorReason: '',
          },
        ],
        TourID: 'tour-1',
        FixPromptPath: 'artifacts/fix.md',
        MarkdownPath: 'artifacts/finding.md',
      },
    ],
    Presentation: {
      Summary: '',
      MarkdownPath: 'artifacts/result.md',
      Overview: {
        ID: 'overview',
        Title: 'Обзор изменений',
        FindingID: '',
        Steps: [
          {
            ID: 'step-1',
            Title: 'Войти в аккаунт',
            Body: 'Нажатие передаёт почту и пароль на проверку.',
            Anchors: [
              {
                FileID: 'file-1',
                StartLine: 42,
                EndLine: 45,
                TaskID: 'TASK-42',
                DesignID: '',
                NoAnchorReason: '',
              },
            ],
            ErrorLocation: false,
          },
          {
            ID: 'step-2',
            Title: 'Показать ответ',
            Body: 'Показываем результат входа.',
            Anchors: [
              {
                FileID: '',
                StartLine: 0,
                EndLine: 0,
                TaskID: 'TASK-42',
                DesignID: '',
                NoAnchorReason: 'Общее требование без привязки к коду.',
              },
            ],
            ErrorLocation: false,
          },
        ],
      },
      FindingTours: [
        {
          ID: 'tour-1',
          Title: 'Повторная отправка',
          FindingID: 'finding-1',
          Steps: [
            {
              ID: 'error-1',
              Title: 'Повторное нажатие',
              Body: 'Пользователь может нажать Войти повторно.',
              Anchors: [
                {
                  FileID: 'file-1',
                  StartLine: 42,
                  EndLine: 45,
                  TaskID: 'TASK-42',
                  DesignID: '',
                  NoAnchorReason: '',
                },
              ],
              ErrorLocation: true,
            },
          ],
        },
      ],
    },
    EvidenceFiles: [],
    EvidenceDesigns: [],
    Activities: [],
    Publications: [],
    Verdict: 'request_changes',
    Error: '',
    StopRequested: false,
    SourceRevision: 'abc',
    ChangeURL: '',
  };
}
