import { graphDrawing } from './graphDrawing';
import type { TextMeasure } from './graphLabel';
import { GraphIcon } from './GraphIcon';
import { graphTopology, isCause, markerState } from './graphModel';
import { DefinitionDetails } from './DefinitionDetails';
import { StatusIcon } from './StatusIcon';
import { CopyIdentity } from './CopyIdentity';
import { ResizableRunList } from './ResizableRunList';
import { useEffect, useMemo, useRef, useState } from 'react';
import { displayVisit } from './displayVisit';
import {
  BaseEdge,
  EdgeLabelRenderer,
  type EdgeProps,
  Background,
  Panel,
  useReactFlow,
  useStore,
  getViewportForBounds,
  type Rect,
  Handle,
  Position,
  ReactFlow,
  type Node,
  type NodeProps,
  type Edge,
} from '@xyflow/react';
import { Card, Icon, Tooltip, useThemeValue } from '@gravity-ui/uikit';
import { Plus, Minus, ArrowsExpand, FileText } from '@gravity-ui/icons';
import { visibleSelection } from './graphViewport';
import { graphLayout, type RoutedEdge } from './graphLayout';
import '@xyflow/react/dist/style.css';
import type { Graph, GraphEdge, GraphNode } from '../types';
import { usePoll } from '../hooks/api';
import { MarkdownDocument, MemoryDialog } from './MarkdownDocument';
import { Trace } from './Trace';
import { ImageExport } from './ImageExport';
import { Button, Dialog, ErrorNotice, statusNames } from './ui';

// Совместимый экспорт координат для потребителей и регрессионных тестов.
export function layout(nodes: GraphNode[], edges: GraphEdge[]) {
  return graphLayout(nodes, edges).positions;
}
// Рисуем рассчитанный маршрут целиком: smoothstep между двумя handles терял
// обходы препятствий Dagre. Подписи получают место ещё на этапе раскладки.
// Ствол заканчивается у основания непрозрачного наконечника. Контакт с рамкой
// вычисляется отдельно от стабильной раскладки и не сдвигает остальные точки.
function RoutedConnection({ id, data }: EdgeProps) {
  const route = data?.route as RoutedEdge;
  if (!route || route.points.length < 2) return null;
  const color = data?.active
    ? 'var(--g-color-text-info)'
    : 'var(--lawa-graph-edge)';
  return (
    <>
      <BaseEdge
        id={id}
        path={String(data?.path || '')}
        interactionWidth={0}
        style={{
          stroke: color,
          strokeWidth: data?.active ? 3 : 1.5,
          strokeDasharray: route.feedback && !data?.active ? '7 5' : undefined,
        }}
      />
      <polygon
        points={String(data?.arrow || '')}
        fill={color}
        pointerEvents="none"
      />
      {route.text.lines.length > 0 && (
        <EdgeLabelRenderer>
          <div
            className="graph-edge-label"
            style={{
              transform: `translate(${route.label.x}px,${route.label.y}px)`,
              width: route.text.width,
              color,
            }}
          >
            {route.text.lines.map((line, i) => (
              <span key={i}>
                {line}
                {i < route.text.lines.length - 1 ? ' ' : ''}
              </span>
            ))}
          </div>
        </EdgeLabelRenderer>
      )}
    </>
  );
}
const edgeTypes = { routed: RoutedConnection };
type CubeNode = Node<
  {
    label: string;
    state: string;
    visit?: number;
    maxVisits?: number;
    icon?: string;
    height: number;
    marker?: string;
    definition?: boolean;
  },
  'cube'
>;
function Cube({ data, selected }: NodeProps<CubeNode>) {
  return (
    <Card
      view="outlined"
      style={{ height: data.height, width: data.marker ? 160 : 220 }}
      className={`cube tone-${data.state} ${selected ? 'selected' : ''} ${data.marker ? 'graph-marker' : ''}`}
    >
      <Handle type="target" position={Position.Top} />
      <div className="cube-heading">
        <GraphIcon name={data.icon} />
        <strong>{data.label}</strong>
      </div>
      {!data.marker && !data.definition && (
        <small>
          {statusNames[data.state] || data.state}
          {data.visit
            ? ` · попытка ${data.visit}${data.maxVisits ? ` из ${data.maxVisits}` : ''}`
            : ''}
        </small>
      )}
      <Handle type="source" position={Position.Bottom} />
    </Card>
  );
}
const nodeTypes = { cube: Cube };

// Контролы Gravity работают внутри провайдера React Flow, сохраняя pan/zoom API.
function GraphControls({
  bounds,
  selected,
}: {
  bounds: Rect;
  selected?: string;
}) {
  const {
    zoomIn,
    zoomOut,
    getViewport,
    setViewport,
    getNode,
    viewportInitialized,
  } = useReactFlow();
  const width = useStore((state) => state.width);
  const height = useStore((state) => state.height);
  // Размеры узлов заданы раскладкой; ждём готовности камеры, а не измерения
  // всех декоративных групп React Flow. Иначе первый fit может не выполняться.
  const started = useRef(false);
  // Только реальная смена размеров/выбора может поправить камеру. Polling,
  // тема и ручное перемещение не запускают fit и не сбрасывают масштаб.
  useEffect(() => {
    if (!viewportInitialized || !width || !height) return;
    const timer = window.setTimeout(() => {
      if (!started.current) {
        started.current = true;
        void setViewport(
          getViewportForBounds(bounds, width, height, 0.1, 1, 0.15),
        );
        return;
      }
      const node = selected ? getNode(selected) : undefined;
      if (!node) return;
      const viewport = getViewport();
      const next = visibleSelection(viewport, node.position, width, height, {
        width: node.measured?.width || node.width || 220,
        height: node.measured?.height || node.height || 80,
      });
      if (next) void setViewport(next);
    }, 100);
    return () => window.clearTimeout(timer);
  }, [
    width,
    height,
    selected,
    viewportInitialized,
    bounds,
    getNode,
    getViewport,
    setViewport,
  ]);
  return (
    <Panel position="bottom-left" className="graph-controls">
      <Button
        aria-label="Приблизить"
        title="Приблизить"
        onClick={() => void zoomIn()}
      >
        <Icon data={Plus} />
      </Button>
      <Button
        aria-label="Отдалить"
        title="Отдалить"
        onClick={() => void zoomOut()}
      >
        <Icon data={Minus} />
      </Button>
      <Button
        aria-label="Показать весь граф"
        title="Показать весь граф"
        onClick={() =>
          void setViewport(
            getViewportForBounds(bounds, width, height, 0.1, 1, 0.15),
          )
        }
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
  definition,
  onSelectionChange,
}: {
  runID: string;
  stepID?: string;
  visitID?: string;
  preview?: Graph;
  definition?: Graph;
  onSelectionChange?: (step: string, visit?: string) => void;
}) {
  const { data, error } = usePoll<Graph>(
    preview || definition ? null : `/api/graph/${encodeURIComponent(runID)}`,
  );
  const graph = definition || preview || data;
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
  const [memoryOpen, setMemoryOpen] = useState(false);
  const [messagesOpen, setMessagesOpen] = useState(false);
  const [choice, setChoice] = useState({
    // Начальный выбор показывает фактическую работу. Дальше он хранится
    // локально: polling не переключает кубик и не перемещает камеру.
    step:
      initialStep ||
      (!graph.Definition
        ? (graph.Executions || []).find((e) =>
            ['starting', 'running', 'waiting_for_approval'].includes(e.State),
          )?.StepID
        : '') ||
      '',
    visit: initialVisit || '',
    source: `${initialStep}/${initialVisit}`,
  });
  const source = `${initialStep}/${initialVisit}`;
  // В определении URL полностью управляет выбором: back на пустой step
  // не должен восстанавливать устаревший локальный выбор до первого перехода.
  const current =
    !graph.Definition && choice.source === source
      ? choice
      : { step: initialStep || '', visit: initialVisit || '', source };
  const selected =
    (graph.Nodes || []).find((node) => node.ID === current.step) ||
    graph.Nodes?.[0];
  const executions = (graph.Executions || []).filter(
    (entry) => entry.StepID === selected?.ID,
  );
  const execution = displayVisit(executions, current.visit);
  const topologyKey = JSON.stringify([
    (graph.Nodes || []).map((n) => ({
      ID: n.ID,
      Title: n.Title || n.Definition?.Character?.name,
      Icon: n.Icon,
      Start: n.Start || n.Definition?.Start,
      MaxVisits: n.MaxVisits,
      OnLimit: n.OnLimit,
      Prompt: '',
      Routes: [],
    })),
    graph.Edges || [],
    graph.Version,
  ]);
  const topology = useMemo(() => {
    const [Nodes, Edges, Version] = JSON.parse(topologyKey) as [
      GraphNode[],
      GraphEdge[],
      number | undefined,
    ];
    return graphTopology({ Nodes, Edges, Version });
  }, [topologyKey]);
  const geometry = useMemo(() => {
    let measure: TextMeasure | undefined;
    if (typeof CanvasRenderingContext2D !== 'undefined') {
      const context = document.createElement('canvas').getContext('2d');
      if (context) {
        const family = getComputedStyle(document.body).fontFamily;
        measure = (text, font) => {
          context.font = `${font === 'title' ? 'bold 12px' : '13px'} ${family}`;
          return context.measureText(text).width;
        };
      }
    }
    return graphLayout(topology.nodes, topology.edges, measure);
  }, [topology]);
  const bounds = useMemo(() => {
    const boxes = [
      ...[...geometry.positions].map(([id, p]) => ({
        ...p,
        ...geometry.sizes.get(id)!,
      })),
      ...geometry.edges.flatMap((edge) => [
        ...edge.points.map((p) => ({ ...p, width: 1, height: 1 })),
        { ...edge.label, width: edge.text.width, height: edge.text.height },
      ]),
    ];
    const x = Math.min(0, ...boxes.map((b) => b.x)) - 4;
    const y = Math.min(0, ...boxes.map((b) => b.y)) - 4;
    return {
      x,
      y,
      width: Math.max(1, ...boxes.map((b) => b.x + b.width)) - x + 4,
      height: Math.max(1, ...boxes.map((b) => b.y + b.height)) - y + 4,
    };
  }, [geometry]);
  const shownVisits = new Map(
    (graph.Nodes || []).map((node) => [
      node.ID,
      node.ID === selected?.ID
        ? execution
        : displayVisit(
            (graph.Executions || []).filter((item) => item.StepID === node.ID),
          ),
    ]),
  );
  const nodes: CubeNode[] = topology.nodes.map((node) => {
    const shown = shownVisits.get(node.ID);
    const marker = node.Marker;
    return {
      id: node.ID,
      type: 'cube',
      position: geometry.positions.get(node.ID)!,
      selected: !marker && selected?.ID === node.ID,
      selectable: !marker,
      focusable: !marker,
      style: geometry.sizes.get(node.ID),
      data: {
        label: marker
          ? { start: 'Начало', succeeded: 'Готово', failed: 'Ошибка' }[marker]
          : node.Title || node.Definition?.Character?.name || node.ID,
        icon: marker
          ? {
              start: 'PlayFill',
              succeeded: 'CircleCheck',
              failed: 'CircleXmarkFill',
            }[marker]
          : node.Icon,
        state: marker
          ? markerState(marker, graph)
          : graph.Definition
            ? 'not_started'
            : shown?.State || 'not_started',
        visit:
          shown && shown.State !== 'skipped'
            ? shown.RunNumber || shown.Visit || 1
            : undefined,
        maxVisits: node.MaxVisits,
        height: geometry.sizes.get(node.ID)!.height,
        marker,
        definition: graph.Definition,
      },
    };
  });
  const drawings = graphDrawing(
    geometry.edges.map((route) => {
      const sourceMarker = topology.nodes.find(
        (n) => n.ID === route.from,
      )?.Marker;
      const targetMarker = topology.nodes.find(
        (n) => n.ID === route.to,
      )?.Marker;
      const active =
        !graph.Definition &&
        route.members.some((member) =>
          targetMarker
            ? graph.State === targetMarker &&
              graph.StopVisitID === shownVisits.get(route.from)?.Key &&
              !!shownVisits.get(route.from)?.DecisionRecord?.applied &&
              shownVisits.get(route.from)?.DecisionRecord?.key === member.Key &&
              shownVisits.get(route.from)?.DecisionRecord?.finish ===
                targetMarker
            : route.to === selected?.ID &&
              isCause(
                member,
                shownVisits.get(route.to),
                graph.Executions || [],
                sourceMarker,
              ),
        );
      return {
        route,
        active,
        sourceSelected: !sourceMarker && selected?.ID === route.from,
        targetSelected: !targetMarker && selected?.ID === route.to,
      };
    }),
  );
  const edges: Edge[] = drawings.map((data, index) => ({
    id: String(index),
    source: data.route.from,
    target: data.route.to,
    type: 'routed',
    selectable: false,
    focusable: false,
    data: { ...data },
  }));
  const select = (step: string, visit = '') => {
    setMemoryOpen(false);
    setMessagesOpen(false);
    setChoice({ step, visit, source });
    onSelectionChange?.(step, visit);
  };
  return (
    <section
      className={`workflow-graph ${graph.Definition ? 'definition-graph' : ''}`}
      aria-label="Граф workflow"
    >
      <ErrorNotice error={error} />
      <ResizableRunList side="right" definition={!!graph.Definition}>
        <div className="graph-area">
          <ReactFlow
            nodes={nodes}
            edges={edges}
            nodeTypes={nodeTypes}
            edgeTypes={edgeTypes}
            nodesDraggable={false}
            nodesConnectable={false}
            minZoom={0.1}
            maxZoom={2}
            onNodeClick={(_, node) => {
              if (node.type === 'cube' && !node.data.marker) select(node.id);
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
            <GraphControls bounds={bounds} selected={selected?.ID} />
          </ReactFlow>
        </div>
        <aside className="cube-details" aria-label="Информация о кубике">
          {graph.Definition ? (
            <DefinitionDetails
              graph={graph}
              selected={selected}
              onSelect={select}
            />
          ) : (
            <>
              <div className="graph-heading">
                <StatusIcon state={graph.State} entity="workflow" />
                <CopyIdentity
                  text={graph.ID}
                  label="Скопировать runId"
                  success="runId скопирован"
                  infoIcon
                />
                <div className="graph-identity">
                  <h1>
                    <CopyIdentity
                      text={graph.Name}
                      label="Скопировать название workflow"
                      success="Название workflow скопировано"
                    />
                  </h1>
                </div>
              </div>

              <div className="cube-details-content">
                {!preview && <ImageExport runID={graph.ID} />}
                <div className="cube-title-row">
                  <StatusIcon
                    state={execution?.State || 'not_started'}
                    entity="cube"
                  />
                  {selected && (
                    <CopyIdentity
                      text={selected.ID}
                      label="Скопировать Cube ID"
                      success="Cube ID скопирован"
                      tooltipPrefix="Cube ID: "
                      infoIcon
                    />
                  )}
                  <Tooltip
                    content={
                      execution?.TraceURL ? 'Сообщения и действия' : 'Логов нет'
                    }
                  >
                    <span className="run-id-trigger">
                      <Button
                        className="run-id-info"
                        view="flat"
                        size="s"
                        aria-label="Сообщения и действия"
                        disabled={!execution?.TraceURL}
                        onClick={() => setMessagesOpen(true)}
                      >
                        <Icon data={FileText} size={18} />
                      </Button>
                    </span>
                  </Tooltip>
                  <h2>
                    {selected ? (
                      <CopyIdentity
                        text={selected.ID}
                        label="Скопировать название кубика"
                        success="Название кубика скопировано"
                      />
                    ) : (
                      'Нет кубиков'
                    )}
                  </h2>
                  {executions.length > 1 && (
                    <nav
                      className="visit-pagination"
                      aria-label="Итерации кубика"
                    >
                      {[...executions]
                        .sort((a, b) => (a.Visit || 1) - (b.Visit || 1))
                        .map((entry) => (
                          <Button
                            key={entry.Key}
                            view="flat"
                            size="s"
                            selected={entry.Key === execution?.Key}
                            aria-current={
                              entry.Key === execution?.Key ? 'page' : undefined
                            }
                            aria-label={`Посещение ${entry.Visit || 1}`}
                            title={`${statusNames[entry.State] || entry.State}${entry.Trigger ? ` · ${entry.Trigger}` : ''}`}
                            onClick={() => select(selected!.ID, entry.Key)}
                          >
                            #{entry.Visit || 1}
                          </Button>
                        ))}
                    </nav>
                  )}
                </div>
                {execution?.Result && (
                  <MarkdownDocument
                    text={execution.Result}
                    label="Результат работы"
                    copyLabel="Скопировать результат"
                    compact
                  />
                )}
                {(execution?.Note || !execution) && (
                  <p className="note">
                    {execution?.Note || 'Кубик ещё не запускался.'}
                  </p>
                )}
                {/* Факты выбранного посещения не смешиваем со статическими маршрутами:
              возможные маршруты доступны на диаграмме и во вкладке «Описание». */}
                {(execution?.Decision ||
                  execution?.Trigger ||
                  graph.StopReason) && (
                  <dl className="visit-facts">
                    {execution?.Decision && (
                      <>
                        <dt>Решение посещения</dt>
                        <dd>{execution.Decision}</dd>
                      </>
                    )}
                    {execution?.Trigger && (
                      <>
                        <dt>Причина перехода</dt>
                        <dd>{execution.Trigger}</dd>
                      </>
                    )}
                    {graph.StopReason && (
                      <>
                        <dt>Причина остановки workflow</dt>
                        <dd>{graph.StopReason}</dd>
                      </>
                    )}
                  </dl>
                )}
                {execution?.MemoryURL && (
                  <div className="actions">
                    <Button onClick={() => setMemoryOpen(true)}>
                      Память кубика
                    </Button>
                  </div>
                )}
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
            </>
          )}
        </aside>
      </ResizableRunList>
    </section>
  );
}
