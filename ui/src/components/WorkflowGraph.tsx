import { useMemo, useState } from 'react';
import {
  Background,
  Controls,
  Handle,
  Position,
  ReactFlow,
  type Node,
  type NodeProps,
  type Edge,
} from '@xyflow/react';
import dagre from '@dagrejs/dagre';
import '@xyflow/react/dist/style.css';
import type { Graph, GraphEdge, GraphNode } from '../types';
import { usePoll } from '../hooks/api';
import { Continuation } from './Continuation';
import { Trace } from './Trace';
import { ErrorNotice, Status, statusNames } from './ui';

// Dagre раскладывает зависимости, развилки и циклы. Только topology участвует
// в раскладке: новые сообщения и состояния не меняют координаты или viewport.
export function layout(nodes: GraphNode[], edges: GraphEdge[]) {
  const graph = new dagre.graphlib.Graph({ multigraph: true })
    .setGraph({
      rankdir: 'LR',
      nodesep: 40,
      ranksep: 75,
      marginx: 25,
      marginy: 25,
    })
    .setDefaultEdgeLabel(() => ({}));
  nodes.forEach((node) => graph.setNode(node.ID, { width: 220, height: 80 }));
  edges.forEach((edge, index) =>
    graph.setEdge(edge.From, edge.To, {}, String(index)),
  );
  dagre.layout(graph);
  return new Map(
    nodes.map((node) => {
      const point = graph.node(node.ID);
      return [node.ID, { x: point.x - 110, y: point.y - 40 }];
    }),
  );
}
type CubeNode = Node<{ label: string; state: string }, 'cube'>;
function Cube({ data, selected }: NodeProps<CubeNode>) {
  return (
    <div
      className={`cube tone-${data.state} ${selected ? 'selected' : ''}`}
      title={data.label}
    >
      <Handle type="target" position={Position.Left} />
      <strong>{data.label}</strong>
      <small>{statusNames[data.state] || data.state}</small>
      <Handle type="source" position={Position.Right} />
    </div>
  );
}
const nodeTypes = { cube: Cube };

export function WorkflowGraph({
  runID,
  stepID,
  visitID,
  preview,
  onSelectionChange,
}: {
  runID: string;
  stepID?: string;
  visitID?: string;
  preview?: Graph;
  onSelectionChange?: (step: string, visit?: string) => void;
}) {
  const { data, error } = usePoll<Graph>(
    preview ? null : `/api/graph/${encodeURIComponent(runID)}`,
  );
  const graph = preview || data;
  if (!graph)
    return (
      <div className="loading">
        <ErrorNotice error={error} />
        {!error && 'Загрузка графа…'}
      </div>
    );
  return (
    <GraphView
      graph={graph}
      error={error}
      initialStep={stepID}
      initialVisit={visitID}
      onSelectionChange={onSelectionChange}
    />
  );
}

// React Flow и sidebar находятся в одном дереве React, без iframe и DOM-портала.
// Выбор конкретного visit независим от последнего статуса узла на схеме.
function GraphView({
  graph,
  error,
  initialStep,
  initialVisit,
  onSelectionChange,
}: {
  graph: Graph;
  error?: string;
  initialStep?: string;
  initialVisit?: string;
  onSelectionChange?: (step: string, visit?: string) => void;
}) {
  const [choice, setChoice] = useState({
    step: initialStep || '',
    visit: initialVisit || '',
    source: `${initialStep}/${initialVisit}`,
  });
  const source = `${initialStep}/${initialVisit}`;
  const current =
    choice.source === source
      ? choice
      : { step: initialStep || '', visit: initialVisit || '', source };
  const selected =
    (graph.Nodes || []).find((node) => node.ID === current.step) ||
    graph.Nodes?.[0];
  const executions = (graph.Executions || []).filter(
    (entry) => entry.StepID === selected?.ID,
  );
  const execution =
    executions.find((entry) => entry.Key === current.visit) ||
    executions.at(-1);
  const topology = JSON.stringify([
    (graph.Nodes || []).map((node) => node.ID),
    graph.Edges || [],
  ]);
  const positions = useMemo(() => {
    const [ids, edges] = JSON.parse(topology) as [string[], GraphEdge[]];
    return layout(
      ids.map((ID) => ({ ID, Prompt: '', Routes: [] })),
      edges,
    );
  }, [topology]);
  const nodes: CubeNode[] = (graph.Nodes || []).map((node) => ({
    id: node.ID,
    type: 'cube',
    position: positions.get(node.ID)!,
    selected: selected?.ID === node.ID,
    data: {
      label: node.ID,
      state:
        (graph.Executions || [])
          .filter((item) => item.StepID === node.ID)
          .at(-1)?.State || 'pending',
    },
  }));
  const edges: Edge[] = (graph.Edges || []).map((edge, index) => ({
    id: String(index),
    source: edge.From,
    target: edge.To,
    label: edge.Label,
    type: 'smoothstep',
  }));
  const select = (step: string, visit = '') => {
    setChoice({ step, visit, source });
    onSelectionChange?.(step, visit);
  };
  return (
    <section className="workflow-graph" aria-label="Граф workflow">
      <div className="graph-heading">
        <div>
          <h1>{graph.Name}</h1>
          <small>Run {graph.ID}</small>
        </div>
        <Status state={graph.State} />
      </div>
      <ErrorNotice error={error} />
      <div className="graph-workspace">
        <div className="graph-area">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            nodesDraggable={false}
            nodesConnectable={false}
            minZoom={0.1}
            maxZoom={2}
            fitView
            fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
            onNodeClick={(_, node) => select(node.id)}
            onNodesChange={(changes) => {
              const chosen = changes.find(
                (change) => change.type === 'select' && change.selected,
              );
              if (chosen?.type === 'select') select(chosen.id);
            }}
            ariaLabelConfig={{
              'controls.zoomIn.ariaLabel': 'Приблизить',
              'controls.zoomOut.ariaLabel': 'Отдалить',
              'controls.fitView.ariaLabel': 'Показать весь граф',
            }}
            colorMode="dark"
            aria-label="Интерактивная схема workflow"
          >
            <Background gap={20} size={1} />
            <Controls showInteractive={false} />
          </ReactFlow>
          <footer className="legend">
            <span className="tone-succeeded">● Готово</span>
            <span className="tone-running">● В работе</span>
            <span className="tone-failed">● Ошибка</span>
            <span className="muted">● Ожидание</span>
          </footer>
        </div>
        <aside className="cube-details" aria-label="Информация о кубике">
          <h2>{selected?.ID || 'Нет кубиков'}</h2>
          <Status state={execution?.State || 'pending'} />
          {executions.length > 1 && (
            <select
              aria-label="Посещение кубика"
              value={execution?.Key || ''}
              onChange={(event) => select(selected!.ID, event.target.value)}
            >
              {executions.map((entry) => (
                <option key={entry.Key} value={entry.Key}>
                  Посещение {entry.Visit || 1} ·{' '}
                  {statusNames[entry.State] || entry.State}
                </option>
              ))}
            </select>
          )}
          <h3>Результат работы</h3>
          {execution?.Result && (
            <pre className="result">{execution.Result}</pre>
          )}
          <p className="note">
            {execution?.Note || (!execution ? 'Кубик ещё не запускался.' : '')}
          </p>
          <div className="muted facts-text">
            {[
              execution?.Decision,
              execution?.Trigger,
              execution?.Attempt ? `Попытка: ${execution.Attempt}` : '',
              ...(selected?.Routes || []),
              graph.StopReason,
            ]
              .filter(Boolean)
              .join('\n')}
          </div>
          {execution?.MemoryURL && (
            <a
              className="button"
              href={execution.MemoryURL}
              target="_blank"
              rel="noopener noreferrer"
            >
              Память кубика ↗
            </a>
          )}
          {selected && (
            <Continuation
              key={`${selected.ID}/${execution?.Key}`}
              cube={execution?.Prompt || selected.Prompt}
              workflow={graph.Prompt}
            />
          )}
          <h3>Сообщения и действия</h3>
          <Trace url={execution?.TraceURL} />
        </aside>
      </div>
    </section>
  );
}
