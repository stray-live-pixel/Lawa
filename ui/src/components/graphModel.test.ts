import { describe, expect, it } from 'vitest';
import { graphTopology, isCause, markerState } from './graphModel';
import type { Execution, Graph, GraphEdge } from '../types';
const visit = (Key: string, StepID: string, Visit = 1): Execution => ({
  Key,
  StepID,
  Visit,
  State: 'succeeded',
  Result: '',
  Note: '',
  Decision: '',
  Trigger: '',
  TraceURL: '',
  MemoryURL: '',
  Prompt: '',
  Attempt: 1,
});
const graph = (State = 'running'): Graph => ({
  ID: 'run',
  Name: 'flow',
  State,
  StopReason: '',
  Prompt: '',
  Nodes: [{ ID: 'a', Start: true, Prompt: '', Routes: [] }],
  Edges: [],
  Executions: [],
});
const route = (From: string, Key?: string): GraphEdge => ({
  From,
  To: 'developer',
  Label: '',
  Key,
});
describe('причины посещения', () => {
  it('попытка 2 выделяет только reviewer, а не qa или старый старт', () => {
    const history = [
      visit('d1', 'developer'),
      visit('r1', 'reviewer'),
      visit('q1', 'qa'),
    ];
    const current = {
      ...visit('d2', 'developer', 2),
      State: 'running',
      Cause: {
        kind: 'decision' as const,
        sourceVisitIds: ['r1'],
        decisionKey: 'changes_requested',
      },
    };
    expect(
      isCause(route('reviewer', 'changes_requested'), current, history),
    ).toBe(true);
    expect(isCause(route('qa', 'changes_requested'), current, history)).toBe(
      false,
    );
    expect(isCause(route('reviewer', 'approve'), current, history)).toBe(false);
    expect(isCause(route('start'), current, history, 'start')).toBe(false);
    // Выбор первого visit возвращает именно его сохранённую причину.
    const first = { ...history[0], Cause: { kind: 'start' as const } };
    expect(isCause(route('start'), first, history, 'start')).toBe(true);
    expect(
      isCause(route('reviewer', 'changes_requested'), first, history),
    ).toBe(false);
  });
  it('join подсвечивает все сохранённые зависимости, skipped и legacy нейтральны', () => {
    const history = [visit('a1', 'a'), visit('b1', 'b')];
    const current = {
      ...visit('d1', 'developer'),
      Cause: { kind: 'after' as const, sourceVisitIds: ['a1', 'b1'] },
    };
    expect(isCause(route('a'), current, history)).toBe(true);
    expect(isCause(route('b'), current, history)).toBe(true);
    expect(isCause(route('c'), current, history)).toBe(false);
    expect(isCause(route('a'), { ...current, State: 'skipped' }, history)).toBe(
      false,
    );
    expect(isCause(route('a'), visit('old', 'developer'), history)).toBe(false);
  });
  it('не выводит причинность из технической попытки или display-текста', () => {
    expect(
      isCause(
        route('reviewer'),
        {
          ...visit('old', 'developer'),
          Attempt: 5,
          Trigger: 'reviewer → developer',
        },
        [],
      ),
    ).toBe(false);
  });
});
describe('визуальные маркеры', () => {
  it('не сталкиваются с пользовательскими ID и не меняют исходную топологию', () => {
    const data = graph();
    data.Nodes!.push({ ID: '@lawa/start', Prompt: '', Routes: [] });
    const topology = graphTopology(data);
    expect(new Set(topology.nodes.map((n) => n.ID)).size).toBe(
      topology.nodes.length,
    );
    expect(data.Nodes).toHaveLength(2);
    expect(topology.nodes.filter((n) => n.Marker)).toHaveLength(3);
  });
  it('успех не появляется при отмене, ошибке, лимите или в определении', () => {
    for (const state of ['running', 'failed', 'cancelled', 'pending'])
      expect(markerState('succeeded', graph(state))).toBe('not_started');
    expect(markerState('succeeded', graph('succeeded'))).toBe('succeeded');
    expect(markerState('failed', graph('failed'))).toBe('failed');
    expect(markerState('failed', graph('cancelled'))).toBe('not_started');
    expect(
      markerState('succeeded', { ...graph('succeeded'), Definition: true }),
    ).toBe('not_started');
  });
  it('создание pending visit не включает начало; выполнение включает', () => {
    const data = graph();
    data.Executions = [{ ...visit('a1', 'a'), State: 'pending' }];
    expect(markerState('start', data)).toBe('not_started');
    data.Executions[0].State = 'running';
    expect(markerState('start', data)).toBe('running');
  });
});
it('отмена до первого turn не включает старт и причинную стрелку', () => {
  const pendingCancel = {
    ...visit('a1', 'a'),
    State: 'cancelled',
    Attempt: 0,
    Cause: { kind: 'start' as const },
  };
  const data = { ...graph('cancelled'), Executions: [pendingCancel] };
  expect(markerState('start', data)).toBe('not_started');
  expect(isCause(route('start'), pendingCancel, [pendingCancel], 'start')).toBe(
    false,
  );
  expect(
    markerState('start', {
      ...data,
      Executions: [{ ...pendingCancel, Attempt: 1 }],
    }),
  ).toBe('running');
});
