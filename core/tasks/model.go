package tasks

import (
	"encoding/json"
	"fmt"
	"time"
)

type Status string

const (
	StatusCreated            Status = "CREATED"
	StatusQueued             Status = "QUEUED"
	StatusAssigned           Status = "ASSIGNED"
	StatusRunning            Status = "RUNNING"
	StatusWaitingForChildren Status = "WAITING_FOR_CHILDREN"
	StatusWaitingForApproval Status = "WAITING_FOR_APPROVAL"
	StatusBlocked            Status = "BLOCKED"
	StatusCompleted          Status = "COMPLETED"
	StatusPartiallyCompleted Status = "PARTIALLY_COMPLETED"
	StatusFailed             Status = "FAILED"
	StatusCancelRequested    Status = "CANCEL_REQUESTED"
	StatusCancelled          Status = "CANCELLED"
	StatusTimedOut           Status = "TIMED_OUT"
)

type Artifact struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MediaType   string `json:"media_type"`
	URI         string `json:"uri"`
	SHA256      string `json:"sha256,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Description string `json:"description,omitempty"`
}

type Task struct {
	ID                string          `json:"id"`
	ParentTaskID      *string         `json:"parent_task_id,omitempty"`
	RootTaskID        string          `json:"root_task_id"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	Goal              string          `json:"goal"`
	TaskType          string          `json:"task_type"`
	Input             json.RawMessage `json:"input"`
	AssignedAgentID   string          `json:"assigned_agent_id"`
	AllowedTools      []string        `json:"allowed_tools"`
	ApprovalPolicy    string          `json:"approval_policy"`
	Status            Status          `json:"status"`
	Progress          int             `json:"progress"`
	ProgressMessage   string          `json:"progress_message,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	StartedAt         *time.Time      `json:"started_at,omitempty"`
	FinishedAt        *time.Time      `json:"finished_at,omitempty"`
	RetryCount        int             `json:"retry_count"`
	MaxRetries        int             `json:"max_retries"`
	TimeoutSeconds    int             `json:"timeout_seconds"`
	Error             string          `json:"error,omitempty"`
	Result            json.RawMessage `json:"result,omitempty"`
	UserFacingSummary string          `json:"user_facing_summary,omitempty"`
	Artifacts         []Artifact      `json:"artifacts"`
	CorrelationID     string          `json:"correlation_id"`
	CausationID       *string         `json:"causation_id,omitempty"`
	IdempotencyKey    string          `json:"idempotency_key"`
	Version           int64           `json:"version"`
	Priority          int             `json:"priority"`
	Dismissed         bool            `json:"dismissed"`
}

type Dependency struct {
	TaskID          string `json:"task_id"`
	DependsOnTaskID string `json:"depends_on_task_id"`
}

var transitions = map[Status]map[Status]bool{
	StatusCreated:            {StatusQueued: true, StatusWaitingForApproval: true, StatusCancelled: true},
	StatusQueued:             {StatusAssigned: true, StatusCancelRequested: true, StatusTimedOut: true},
	StatusAssigned:           {StatusRunning: true, StatusQueued: true, StatusCancelRequested: true},
	StatusRunning:            {StatusWaitingForChildren: true, StatusWaitingForApproval: true, StatusCompleted: true, StatusPartiallyCompleted: true, StatusFailed: true, StatusCancelRequested: true, StatusTimedOut: true},
	StatusWaitingForChildren: {StatusRunning: true, StatusCompleted: true, StatusPartiallyCompleted: true, StatusFailed: true, StatusCancelRequested: true},
	StatusWaitingForApproval: {StatusQueued: true, StatusCancelled: true, StatusTimedOut: true},
	StatusBlocked:            {StatusQueued: true, StatusCancelled: true},
	StatusCancelRequested:    {StatusCancelled: true, StatusCompleted: true, StatusFailed: true},
}

func CanTransition(from, to Status) bool {
	if from == to {
		return true
	}
	return transitions[from][to]
}

func ValidateTransition(from, to Status) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid task transition %s -> %s", from, to)
	}
	return nil
}

func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusPartiallyCompleted, StatusFailed, StatusCancelled, StatusTimedOut:
		return true
	default:
		return false
	}
}
