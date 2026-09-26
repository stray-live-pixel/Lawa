// Package reviewstore хранит самостоятельное ревью и все его доказательства в одном
// каталоге запуска. Снимок атомарен; артефакты адресуются относительными путями.
package reviewstore

import "time"

// StageID задаёт последовательные этапы; настройки фиксируются перед запуском.
type StageID string

const (
	ContextStage      StageID = "context"
	ReviewStage       StageID = "review"
	PresentationStage StageID = "presentation"
)

// State описывает исполнение, а Verdict — содержательный итог проверки.
type State string

const (
	Pending     State = "pending"
	Running     State = "running"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Stopped     State = "stopped"
	Interrupted State = "interrupted"
)

// AgentConfig хранит фактические параметры отдельного этапа.
type AgentConfig struct {
	Model  string
	Effort string
}

// Config — неизменяемый снимок настроек запуска. Цены опциональны: отсутствие
// тарифа не означает бесплатного использования подписки.
type Config struct {
	// CodexExecutable сохраняет явный выбор CLI для последующих попыток из UI.
	CodexExecutable string
	Context         AgentConfig
	Review          AgentConfig
	Presentation    AgentConfig
	Prices          map[string]Pricing
	RublesPerDollar float64
}

// Pricing задаёт API-эквивалент за миллион токенов и происхождение тарифа.
type Pricing struct {
	Basis       string // Границы оценки: режим тарификации, исключённые надбавки и инструменты.
	Input       float64
	CachedInput float64
	Output      float64
	Source      string
	AsOf        string
}

// Usage не считает кэш дважды: InputTokens включает CachedInputTokens.
// nil означает, что источник не сообщил показатель, а не нулевой расход.
type Usage struct {
	InputTokens       *int64
	CachedInputTokens *int64
	OutputTokens      *int64
	CostUSD           *float64
	Complete          bool
}

// Attempt сохраняет историю повторов и ссылки на фактические запросы/ответы.
// ThreadUsage хранит отдельную группу счётчиков потока и модели без двойного
// учёта агрегата родителя; Attempt.Usage содержит итог по всем этим группам.
type ThreadUsage struct {
	ThreadID string
	Model    string
	Effort   string
	Usage    Usage
}

type Attempt struct {
	ThreadUsage []ThreadUsage
	Number      int
	State       State
	StartedAt   time.Time
	FinishedAt  *time.Time
	ThreadID    string
	TurnID      string
	PromptPath  string
	OutputPath  string
	Error       string
	Usage       Usage
}

// Stage содержит отдельную историю исполнения; повтор не перезаписывает попытки.
type Stage struct {
	ID       StageID
	State    State
	Attempts []Attempt
}

// Link не привязывает интерфейс к GitHub или другому поставщику инфраструктуры.
type Link struct {
	Label string
	URL   string
}

// Task хранит исходное требование с комментариями; порядок определяет приоритет.
type Task struct {
	ID       string
	Title    string
	URL      string
	BodyPath string
	Comments []Comment
}

// Comment — сохранённый текст обсуждения задачи.
type Comment struct {
	ID        string
	Author    string
	CreatedAt string
	BodyPath  string
	URL       string
}

// Design — скачанный снимок, а не повторно сгенерированная картинка.
type Design struct {
	ID          string
	Title       string
	Path        string
	SourceURL   string
	Version     string
	CollectedAt time.Time
}

// File хранит прочитанный снимок, диапазон строк и происхождение изменения.
// LineChange задаёт цвет сохранённых строк: добавление, удаление или контекст.
type LineChange struct {
	StartLine int
	EndLine   int
	Kind      string
}

type File struct {
	LineChanges  []LineChange
	ID           string
	Path         string
	SnapshotPath string
	StartLine    int
	EndLine      int
	Change       string
	AddedLines   int
	DeletedLines int
	Revision     string
}

// Command хранит воспроизводимую команду и её фактический вывод.
type Command struct {
	ID         string
	Command    string
	CWD        string
	ScriptPath string
	OutputPath string
	ExitCode   *int
	ExecutedAt time.Time
}

// ChangeStats разделяет изменения строк и файлов, включая модифицированные файлы.
type ChangeStats struct {
	AddedLines    int
	DeletedLines  int
	AddedFiles    int
	DeletedFiles  int
	ModifiedFiles int
}

// Context фиксируется после сбора. Дополнительные доказательства проверки
// сохраняются отдельно в Review.EvidenceFiles/EvidenceDesigns, не меняя базу.
type Context struct {
	Tasks        []Task
	Designs      []Design
	Changes      []File
	ProjectFiles []File
	Commands     []Command
	DiffPath     string
	Links        []Link
	Stats        ChangeStats
	FrozenAt     *time.Time
}

// Anchor связывает пояснение с сохранённым кодом, задачей или изображением.
// Отсутствие связи явно объясняется в NoAnchorReason.
type Anchor struct {
	FileID         string
	StartLine      int
	EndLine        int
	TaskID         string
	DesignID       string
	NoAnchorReason string
}

// Finding сохраняется проверяющим агентом до отдельного этапа презентации.
type Finding struct {
	ID            string
	Priority      string
	Title         string
	Explanation   string
	Reproduction  string
	Consequence   string
	Anchors       []Anchor
	TourID        string
	FixPromptPath string
	MarkdownPath  string
}

// Tour — смысловой маршрут обзора изменений или доказательства замечания.
type Tour struct {
	ID        string
	Title     string
	FindingID string
	Steps     []TourStep
}

// TourStep содержит одну понятную мысль и точные опорные материалы.
type TourStep struct {
	ID            string
	Title         string
	Body          string
	Anchors       []Anchor
	ErrorLocation bool
}

// Presentation — единый источник viewer и публикации Markdown.
type Presentation struct {
	Overview     Tour
	FindingTours []Tour
	Summary      string
	MarkdownPath string
}

// Activity регистрирует наблюдённые чтения скиллов и реально запущенных субагентов.
type Activity struct {
	Stage          StageID
	Attempt        int
	Model          string
	Effort         string
	ID             string
	Kind           string
	Title          string
	State          string
	DocumentPath   string
	ThreadID       string
	ParentThreadID string
	At             time.Time
}

// Publication связывает опубликованный комментарий с замечанием и ревизией.
type Publication struct {
	ContentHash string // SHA-256 опубликованного Markdown связывает receipt с точной версией.
	FindingID   string
	URL         string
	ExternalID  string
	Revision    string
	State       string
	Error       string
	PublishedAt *time.Time
}

// Review — авторитетный снимок сущности. JSON использует имена полей Go, как API
// dashboard; Version обеспечивает явную миграцию формата в будущем.
type Review struct {
	Version         int
	Kind            string
	ID              string
	CWD             string
	Prompt          string
	Title           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Revision        uint64
	State           State
	CurrentStage    StageID
	Config          Config
	Stages          []Stage
	Context         Context
	Findings        []Finding
	Presentation    Presentation
	EvidenceFiles   []File
	EvidenceDesigns []Design
	Activities      []Activity
	Publications    []Publication
	Verdict         string
	Error           string
	StopRequested   bool
	SourceRevision  string
	ChangeURL       string
}

// Event хранится отдельным неизменяемым файлом; Data — исходное содержимое события.
type Event struct {
	ID      string
	At      time.Time
	Stage   StageID
	Kind    string
	Message string
	Data    any
}

// ViewPatch обновляет только переданные поля навигации. nil сохраняет прежнее
// значение; указатель на пустую строку явно закрывает выбранную экскурсию/шаг.
type ViewPatch struct {
	SeenRevision  *uint64
	SelectedStage *StageID
	SelectedTour  *string
	SelectedStep  *string
}

// ViewState отделён от ревью: чтение не меняет UpdatedAt и порядок истории.
type ViewState struct {
	SeenRevision  uint64
	SelectedStage StageID
	SelectedTour  string
	SelectedStep  string
	UpdatedAt     time.Time
}

// CreateOptions содержит только входы нового запуска.
type CreateOptions struct {
	CWD    string
	Prompt string
	Config Config
}
