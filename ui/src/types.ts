// Контракт read-only API Go. Внешние поля сохраняют имена DTO; nullable-массивы
// нормализуются у потребителя. Внутренние поля runstore сюда не передаются.
export interface Step {
  Result?: string;
  Key: string;
  ID: string;
  StepID: string;
  VisitID: string;
  State: string;
  Tone: string;
  Runtime: string;
  Message: string;
  Action: string;
  Updated: string;
  Trigger: string;
  Decision: string;
  Explanation: string;
  Transition: string;
  Skipped: string;
  Limit: string;
  TechnicalError: string;
  DecisionError: string;
  Visit: number;
  Iteration: number;
  Attempt: number;
  EventsURL: string;
  MemoryURL: string;
  TraceURL: string;
  HasMemory: boolean;
  Active: boolean;
}
export interface Run {
  ID: string;
  ParentID: string;
  Name: string;
  State: string;
  Tone: string;
  Updated: string;
  TicketID: string;
  TicketTitle: string;
  TicketURL: string;
  StopReason: string;
  StopVisit: string;
  StopLimit: string;
  EventsURL: string;
  VSCodeURL: string;
  DeleteURL: string;
  Open: boolean;
  HasUnfinished: boolean;
  HasWorking: boolean;
  HasFailed: boolean;
  AgentGraph: boolean;
  CompletedSteps: number;
  TotalSteps: number;
  Steps: Step[] | null;
  ActiveSteps: Step[] | null;
  Children: Run[] | null;
}
export interface ScheduledRun {
  SeriesID: string;
  WorkflowID: string;
  Next: string;
  Remaining: string;
  Schedule: string;
  Progress: string;
  Overdue: boolean;
}
export interface Dashboard {
  Title: string;
  Refresh: string;
  EmptyMessage: string;
  Preview: boolean;
  Roots: Run[] | null;
  Scheduled: ScheduledRun[] | null;
  Problems: { Name: string; Message: string }[] | null;
  Filter: {
    Query: string;
    WindowLabel: string;
    Scope: string;
    Period: string;
    States: string;
    RootID: string;
    ActiveURL: string;
    AllURL: string;
    AllStatesURL: string;
    WorkingURL: string;
    FailedURL: string;
    Periods: { Value: string; Label: string; Selected: boolean }[];
    FocusPath:
      { ID: string; Name: string; State: string; Tone: string }[] | null;
    FocusParentID: string;
    Total: number;
    SearchMatched: number;
    Matched: number;
    HasActiveQuery: boolean;
    ActiveOnly: boolean;
    WorkingOnly: boolean;
    FailedOnly: boolean;
    Focused: boolean;
  };
  Pagination: {
    Current: number;
    Total: number;
    PreviousURL: string;
    NextURL: string;
    Items: { Label: string; URL: string; Current: boolean }[] | null;
    Visible: boolean;
  };
}
export interface GraphNode {
  ID: string;
  Prompt: string;
  Routes: string[] | null;
}
export interface GraphEdge {
  From: string;
  To: string;
  Label: string;
}
export interface Execution {
  Key: string;
  StepID: string;
  State: string;
  Result: string;
  Note: string;
  Decision: string;
  Trigger: string;
  TraceURL: string;
  MemoryURL: string;
  Prompt: string;
  Visit: number;
  Attempt: number;
}
export interface Graph {
  ID: string;
  Name: string;
  State: string;
  StopReason: string;
  Prompt: string;
  Nodes: GraphNode[] | null;
  Edges: GraphEdge[] | null;
  Executions: Execution[] | null;
}
export interface TraceEvent {
  time: string;
  kind: string;
  itemId?: string;
  itemType?: string;
  content?: string;
  message?: string;
  turnId?: string;
}
export interface Trace {
  events: TraceEvent[] | null;
  next: number;
}
