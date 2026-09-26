// DTO файлового reviewstore. Отсутствующие метрики не заменяются нулями.
export type StageID = 'context' | 'review' | 'presentation';
export type State =
  'pending' | 'running' | 'succeeded' | 'failed' | 'stopped' | 'interrupted';
export interface AgentConfig {
  Model: string;
  Effort: string;
}
export interface Config {
  CodexExecutable?: string;
  Context: AgentConfig;
  Review: AgentConfig;
  Presentation: AgentConfig;
  Prices: Record<string, Pricing>;
  RublesPerDollar: number;
}
export interface Pricing {
  Basis?: string;
  Input: number;
  CachedInput: number;
  Output: number;
  Source: string;
  AsOf: string;
}
export interface Usage {
  InputTokens: number | null;
  CachedInputTokens: number | null;
  OutputTokens: number | null;
  CostUSD: number | null;
  Complete: boolean;
}
export interface ThreadUsage {
  ThreadID: string;
  Model: string;
  Effort: string;
  Usage: Usage;
}
export interface Attempt {
  ThreadUsage?: ThreadUsage[] | null;
  Number: number;
  State: State;
  StartedAt: string;
  FinishedAt: string | null;
  ThreadID: string;
  TurnID: string;
  PromptPath: string;
  OutputPath: string;
  Error: string;
  Usage: Usage;
}
export interface Stage {
  ID: StageID;
  State: State;
  Attempts: Attempt[] | null;
}
export interface Link {
  Label: string;
  URL: string;
}
export interface Task {
  ID: string;
  Title: string;
  URL: string;
  BodyPath: string;
  Comments: Comment[] | null;
}
export interface Comment {
  ID: string;
  Author: string;
  CreatedAt: string;
  BodyPath: string;
  URL: string;
}
export interface Design {
  ID: string;
  Title: string;
  Path: string;
  SourceURL: string;
  Version: string;
  CollectedAt: string;
}
export interface LineChange {
  StartLine: number;
  EndLine: number;
  Kind: string;
}
export interface File {
  LineChanges?: LineChange[] | null;
  ID: string;
  Path: string;
  SnapshotPath: string;
  StartLine: number;
  EndLine: number;
  Change: string;
  AddedLines: number;
  DeletedLines: number;
  Revision: string;
}
export interface Command {
  ID: string;
  Command: string;
  CWD: string;
  ScriptPath: string;
  OutputPath: string;
  ExitCode: number | null;
  ExecutedAt: string;
}
export interface ChangeStats {
  AddedLines: number;
  DeletedLines: number;
  AddedFiles: number;
  DeletedFiles: number;
  ModifiedFiles: number;
}
export interface Context {
  Tasks: Task[] | null;
  Designs: Design[] | null;
  Changes: File[] | null;
  ProjectFiles: File[] | null;
  Commands: Command[] | null;
  DiffPath: string;
  Links: Link[] | null;
  Stats: ChangeStats;
  FrozenAt: string | null;
}
export interface Anchor {
  FileID: string;
  StartLine: number;
  EndLine: number;
  TaskID: string;
  DesignID: string;
  NoAnchorReason: string;
}
export interface Finding {
  ID: string;
  Priority: string;
  Title: string;
  Explanation: string;
  Reproduction: string;
  Consequence: string;
  Anchors: Anchor[] | null;
  TourID: string;
  FixPromptPath: string;
  MarkdownPath: string;
}
export interface Tour {
  ID: string;
  Title: string;
  FindingID: string;
  Steps: TourStep[] | null;
}
export interface TourStep {
  ID: string;
  Title: string;
  Body: string;
  Anchors: Anchor[] | null;
  ErrorLocation: boolean;
}
export interface Presentation {
  Overview: Tour;
  FindingTours: Tour[] | null;
  Summary: string;
  MarkdownPath: string;
}
export interface Activity {
  Stage?: StageID;
  Attempt?: number;
  Model?: string;
  Effort?: string;
  ID: string;
  Kind: string;
  Title: string;
  State: string;
  DocumentPath: string;
  ThreadID: string;
  ParentThreadID: string;
  At: string;
}
export interface Publication {
  FindingID: string;
  URL: string;
  ExternalID: string;
  Revision: string;
  State: string;
  Error: string;
  PublishedAt: string | null;
}
export interface Review {
  Version: number;
  Kind: string;
  ID: string;
  CWD: string;
  Prompt: string;
  Title: string;
  CreatedAt: string;
  UpdatedAt: string;
  Revision: number;
  State: State;
  CurrentStage: StageID;
  Config: Config;
  Stages: Stage[] | null;
  Context: Context;
  Findings: Finding[] | null;
  Presentation: Presentation;
  EvidenceFiles: File[] | null;
  EvidenceDesigns: Design[] | null;
  Activities: Activity[] | null;
  Publications: Publication[] | null;
  Verdict: string;
  Error: string;
  StopRequested: boolean;
  SourceRevision: string;
  ChangeURL: string;
}
export interface Event {
  ID: string;
  At: string;
  Stage: StageID;
  Kind: string;
  Message: string;
  Data: unknown;
}
export interface ViewState {
  SeenRevision: number;
  SelectedStage: StageID;
  SelectedTour: string;
  SelectedStep: string;
  UpdatedAt: string;
}
export interface CreateOptions {
  CWD: string;
  Prompt: string;
  Config: Config;
}
