import { ResizableRunList } from './ResizableRunList';
import { useMemo, useState } from 'react';
import { displayVisit } from './displayVisit';
import {
  BaseEdge,
  EdgeLabelRenderer,
  type EdgeProps,
  Background,
  Panel,
  useReactFlow,
  Handle,
  MarkerType,
  Position,
  ReactFlow,
  type Node,
  type NodeProps,
  type Edge,
} from '@xyflow/react';
import { Card, ClipboardButton, Icon, useThemeValue } from '@gravity-ui/uikit';
import {
  Plus,
  Minus,
  ArrowsExpand,
  ChevronsExpandUpRight,
  ChevronsCollapseUpRight,
} from '@gravity-ui/icons';
import { graphLayout, type RoutedEdge } from './graphLayout';
import '@xyflow/react/dist/style.css';
import type { Graph, GraphEdge, GraphNode } from '../types';
import { usePoll } from '../hooks/api';
import { MarkdownDocument, MemoryDialog } from './MarkdownDocument';
import { Trace } from './Trace';
import { ImageExport } from './ImageExport';
import { Button, Choice, Dialog, ErrorNotice, Status, statusNames } from './ui';

// Совместимый экспорт координат для потребителей и регрессионных тестов.
export function layout(nodes: GraphNode[], edges: GraphEdge[]) {
  return graphLayout(nodes, edges).positions;
}
// Рисуем рассчитанный маршрут целиком: smoothstep между двумя handles терял
// обходы препятствий Dagre. Подписи получают место ещё на этапе раскладки.
function RoutedConnection({ id, data, markerEnd }: EdgeProps) {
  const route = data?.route as RoutedEdge;
  if (!route) return null;
  const path = route.points
    .map((p, i) => `${i ? 'L' : 'M'} ${p.x},${p.y}`)
    .join(' ');
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={markerEnd}
        style={{
          stroke: 'var(--g-color-text-secondary)',
          strokeWidth: 1.5,
          strokeDasharray: route.feedback ? '7 5' : undefined,
        }}
      />
      {(route.labels.length > 0 || route.feedback) && (
        <EdgeLabelRenderer>
          <div
            className="graph-edge-label"
            style={{
              transform: `translate(-50%, -50%) translate(${route.label.x}px,${route.label.y}px)`,
            }}
          >
            {route.feedback ? '↩ Возврат' : ''}
            {route.feedback && route.labels.length ? ' · ' : ''}
            {route.labels.join(' · ')}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
function WorkflowGroup({ data }: NodeProps) {
  return (
    <div className="workflow-group">
      <span>{String(data.label)}</span>
    </div>
  );
}
const edgeTypes = { routed: RoutedConnection };
type CubeNode = Node<{ label: string; state: string; visit?: number }, 'cube'>;
function Cube({ data, selected }: NodeProps<CubeNode>) {
  return (
    <Card
      view="outlined"
      className={`cube tone-${data.state} ${selected ? 'selected' : ''}`}
      title={data.label}
    >
      <Handle type="target" position={Position.Top} />
      <strong>{data.label}</strong>
      <small>
        {statusNames[data.state] || data.state}
        {data.visit ? ` · #${data.visit}` : ''}
      </small>
      <Handle type="source" position={Position.Bottom} />
    </Card>
  );
}
const nodeTypes = { cube: Cube, workflowGroup: WorkflowGroup };

// Контролы Gravity работают внутри провайдера React Flow, сохраняя pan/zoom API.
function GraphControls() {
  const { zoomIn, zoomOut, fitView } = useReactFlow();
  return (
    <Panel position="bottom-left" className="graph-controls">
      <Button aria-label="Приблизить" onClick={() => void zoomIn()}>
        <Icon data={Plus} />
      </Button>
      <Button aria-label="Отдалить" onClick={() => void zoomOut()}>
        <Icon data={Minus} />
      </Button>
      <Button
        aria-label="Показать весь граф"
        onClick={() => void fitView({ padding: 0.15, maxZoom: 1 })}
      >
        <Icon data={ArrowsExpand} />
      </Button>
    </Panel>
  );
}

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
      preview={!!preview}
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
  preview,
  error,
  initialStep,
  initialVisit,
  onSelectionChange,
}: {
  graph: Graph;
  preview?: boolean;
  error?: string;
  initialStep?: string;
  initialVisit?: string;
  onSelectionChange?: (step: string, visit?: string) => void;
}) {
  const theme = useThemeValue();
  const [expanded, setExpanded] = useState(false);
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [messagesOpen, setMessagesOpen] = useState(false);
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
  const execution = displayVisit(executions, current.visit);
  const topology = JSON.stringify([
    (graph.Nodes || []).map((node) => node.ID),
    graph.Edges || [],
  ]);
  const geometry = useMemo(() => {
    const [ids, edges] = JSON.parse(topology) as [string[], GraphEdge[]];
    return graphLayout(
      ids.map((ID) => ({ ID, Prompt: '', Routes: [] })),
      edges,
    );
  }, [topology]);
  const nodes: CubeNode[] = (graph.Nodes || []).map((node) => {
    // Для выбранного кубика цвет, подпись и детали используют один visit.
    // Остальные узлы автоматически показывают актуальную реальную работу.
    const shown =
      node.ID === selected?.ID
        ? execution
        : displayVisit(
            (graph.Executions || []).filter((item) => item.StepID === node.ID),
          );
    return {
      id: node.ID,
      type: 'cube',
      position: geometry.positions.get(node.ID)!,
      selected: selected?.ID === node.ID,
      data: {
        label: node.ID,
        state: shown?.State || 'not_started',
        visit: shown ? shown.Visit || 1 : undefined,
      },
    };
  });
  const groups: Node[] = geometry.groups.map((group) => ({
    id: group.id,
    type: 'workflowGroup',
    position: { x: group.x, y: group.y },
    style: { width: group.width, height: group.height },
    data: { label: group.label },
    selectable: false,
    draggable: false,
    focusable: false,
    zIndex: -1,
  }));
  const edges: Edge[] = geometry.edges.map((route, index) => ({
    id: String(index),
    source: route.from,
    target: route.to,
    type: 'routed',
    data: { route },
    markerEnd: {
      type: MarkerType.ArrowClosed,
      width: 18,
      height: 18,
      color: 'var(--g-color-text-secondary)',
    },
  }));
  const select = (step: string, visit = '') => {
    setMemoryOpen(false);
    setMessagesOpen(false);
    setChoice({ step, visit, source });
    onSelectionChange?.(step, visit);
  };
  return (
    <section
      className={`workflow-graph ${expanded ? 'workflow-graph-expanded' : ''}`}
      aria-label="Граф workflow"
    >
      <div className="graph-heading">
        <div className="graph-identity">
          <div className="graph-title-row">
            <Status state={graph.State} />
            <h1>{graph.Name}</h1>
          </div>
          <div className="graph-run-id">
            <small>Run {graph.ID}</small>
            <ClipboardButton
              text={graph.ID}
              size="xs"
              view="flat"
              aria-label="Скопировать ID запуска"
              tooltipInitialText="Скопировать ID запуска"
              tooltipSuccessText="ID скопирован"
            />
          </div>
        </div>
        <Button
          view="flat"
          onClick={() => setExpanded(!expanded)}
          aria-pressed={expanded}
          aria-label={expanded ? 'Свернуть граф' : 'Развернуть граф'}
          title={expanded ? 'Свернуть граф' : 'Развернуть граф'}
        >
          <Icon
            data={expanded ? ChevronsCollapseUpRight : ChevronsExpandUpRight}
          />
        </Button>
      </div>
      <ErrorNotice error={error} />
      <ResizableRunList side="right">
        <div className="graph-area">
          <ReactFlow
            nodes={[...groups, ...nodes]}
            edges={edges}
            nodeTypes={nodeTypes}
            edgeTypes={edgeTypes}
            nodesDraggable={false}
            nodesConnectable={false}
            minZoom={0.1}
            maxZoom={2}
            fitView
            fitViewOptions={{ padding: 0.15, maxZoom: 1 }}
            onNodeClick={(_, node) => {
              if (node.type === 'cube') select(node.id);
            }}
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
            colorMode={theme === 'dark' ? 'dark' : 'light'}
            aria-label="Интерактивная схема workflow"
          >
            <Background gap={20} size={1} />
            <GraphControls />
          </ReactFlow>
        </div>
        <aside className="cube-details" aria-label="Информация о кубике">
          {!preview && <ImageExport runID={graph.ID} />}
          <h2>{selected?.ID || 'Нет кубиков'}</h2>
          <Status state={execution?.State || 'not_started'} />
          {execution && (
            <p className="muted">Посещение #{execution.Visit || 1}</p>
          )}
          {executions.length > 0 && (
            <Choice
              aria-label="Посещение кубика"
              value={current.visit || 'auto'}
              onUpdate={(value) =>
                select(selected!.ID, value === 'auto' ? '' : value)
              }
              options={[
                { value: 'auto', content: 'Актуальное посещение' },
                ...executions.map((entry) => ({
                  value: entry.Key,
                  content: `Посещение ${entry.Visit || 1} · ${statusNames[entry.State] || entry.State}${entry.Trigger ? ` · ${entry.Trigger}` : ''}`,
                })),
              ]}
            />
          )}
          <h3>Результат работы</h3>
          {execution?.Result && (
            <MarkdownDocument
              text={execution.Result}
              label="Результат работы"
              copyLabel="Скопировать результат"
            />
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
          <div className="actions">
            {execution?.MemoryURL && (
              <Button onClick={() => setMemoryOpen(true)}>Память кубика</Button>
            )}
            <Button
              disabled={!execution?.TraceURL}
              onClick={() => setMessagesOpen(true)}
            >
              Сообщения и действия
            </Button>
          </div>
          <MemoryDialog
            key={`memory/${execution?.Key}`}
            url={execution?.MemoryURL}
            open={memoryOpen}
            onOpenChange={setMemoryOpen}
          />
          <Dialog
            open={messagesOpen}
            onOpenChange={setMessagesOpen}
            title={`Сообщения и действия · ${selected?.ID || ''}`}
          >
            {messagesOpen && (
              <Trace key={execution?.Key} url={execution?.TraceURL} />
            )}
          </Dialog>
        </aside>
      </ResizableRunList>
    </section>
  );
}
