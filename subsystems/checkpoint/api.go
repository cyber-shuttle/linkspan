package checkpoint

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/utils"
	"github.com/gorilla/mux"
)

// GlobalCheckpointService is what the handlers act on. main.go installs it at
// startup; the handlers refuse politely until it is there, so an allocation
// started without checkpoint flags still serves the rest of the API.
var GlobalCheckpointService *CheckpointService

// CheckpointCreateRequest is the JSON body for POST /checkpoints. Exactly one
// of ProcessID and PID identifies the target.
type CheckpointCreateRequest struct {
	ProcessID    string `json:"process_id,omitempty"`
	PID          int    `json:"pid,omitempty"`
	WorkloadID   string `json:"workload_id,omitempty"`   // defaults to this allocation's
	Mode         string `json:"mode,omitempty"`          // cpu | gpu | auto
	Trigger      string `json:"trigger,omitempty"`       // manual | workflow | walltime | signal
	LeaveRunning *bool  `json:"leave_running,omitempty"` // defaults by trigger
	Reason       string `json:"reason,omitempty"`
}

// CheckpointRestoreRequest is the JSON body for POST /checkpoints/{id}/restore.
// Every field is optional; an omitted one falls back to the flag this
// allocation was started with.
type CheckpointRestoreRequest struct {
	Force                *bool    `json:"force,omitempty"`
	ShutdownOnCompletion *bool    `json:"shutdown_on_completion,omitempty"`
	PreRestoreCommands   []string `json:"pre_restore_commands,omitempty"`
	EnsureDirs           []string `json:"ensure_dirs,omitempty"`
	RequireFiles         []string `json:"require_files,omitempty"`
}

// CheckpointResponse is the REST view of a checkpoint, built from the manifest
// so a freshly created checkpoint and a listed one look identical.
type CheckpointResponse struct {
	CheckpointID string            `json:"checkpoint_id"`
	WorkloadID   string            `json:"workload_id"`
	State        CheckpointState   `json:"state"`
	Trigger      CheckpointTrigger `json:"trigger"`
	Mode         CheckpointMode    `json:"mode"`
	LeaveRunning bool              `json:"leave_running"`
	Reason       string            `json:"reason,omitempty"`
	ProcessID    string            `json:"process_id,omitempty"`
	PID          int               `json:"pid,omitempty"`
	Command      string            `json:"command,omitempty"`
	WorkingDir   string            `json:"working_dir,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	CompletedAt  time.Time         `json:"completed_at,omitempty"`
	GPUMode      bool              `json:"gpu_mode"`
	SlurmJobID   string            `json:"slurm_job_id,omitempty"`
	SlurmNode    string            `json:"slurm_node,omitempty"`
}

// CheckpointRestoreResponse identifies the restored application — not the CRIU
// command that restored it.
type CheckpointRestoreResponse struct {
	CheckpointID string    `json:"checkpoint_id"`
	WorkloadID   string    `json:"workload_id"`
	ProcessID    string    `json:"process_id"`
	PID          int       `json:"pid"`
	RestoredAt   time.Time `json:"restored_at"`
	Warnings     []string  `json:"warnings,omitempty"`
}

func checkpointResponse(m *Manifest) CheckpointResponse {
	return CheckpointResponse{
		CheckpointID: m.CheckpointID,
		WorkloadID:   m.WorkloadID,
		State:        m.State,
		Trigger:      m.Trigger,
		Mode:         m.Mode,
		LeaveRunning: m.LeaveRunning,
		Reason:       m.Reason,
		ProcessID:    m.ProcessID,
		PID:          m.OriginalPID,
		Command:      m.Command,
		WorkingDir:   m.WorkingDir,
		CreatedAt:    m.CreatedAt,
		CompletedAt:  m.CompletedAt,
		GPUMode:      m.GPUMode,
		SlurmJobID:   m.SlurmJobID,
		SlurmNode:    m.SlurmNode,
	}
}

func respondErrorJSON(w http.ResponseWriter, status int, msg string) {
	utils.RespondJSON(w, status, map[string]string{"error": msg})
}

// respondServiceError maps a service error onto the closest status: a busy
// workload is a conflict and an unknown checkpoint a 404, both of which a
// client can act on, while anything else is ours to answer for.
func respondServiceError(w http.ResponseWriter, err error, fallback int) {
	switch {
	case errors.Is(err, ErrWorkloadBusy):
		respondErrorJSON(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrCheckpointNotFound):
		respondErrorJSON(w, http.StatusNotFound, err.Error())
	default:
		respondErrorJSON(w, fallback, err.Error())
	}
}

// activeService resolves the service and reports whether it can actually work,
// answering 503 when this allocation was started without checkpoint flags.
func activeService(w http.ResponseWriter) (*CheckpointService, bool) {
	svc := GlobalCheckpointService
	if svc == nil {
		respondErrorJSON(w, http.StatusServiceUnavailable, "checkpoint service is not available")
		return nil, false
	}
	if err := svc.Configured(); err != nil {
		respondErrorJSON(w, http.StatusServiceUnavailable, err.Error())
		return nil, false
	}
	return svc, true
}

// CreateCheckpointHandler handles POST /checkpoints to checkpoint a running
// process by linkspan process id or by OS pid.
func CreateCheckpointHandler(w http.ResponseWriter, r *http.Request) {
	svc, ok := activeService(w)
	if !ok {
		return
	}

	var req CheckpointCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondErrorJSON(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if (req.ProcessID == "") == (req.PID == 0) {
		respondErrorJSON(w, http.StatusBadRequest, "exactly one of process_id and pid is required")
		return
	}
	target := TargetFromProcessID(req.ProcessID)
	if req.ProcessID == "" {
		target = TargetFromPID(req.PID)
	}

	workloadID := req.WorkloadID
	if workloadID == "" {
		workloadID = svc.DefaultWorkloadID()
	}
	if workloadID == "" {
		respondErrorJSON(w, http.StatusBadRequest, "workload_id is required: this allocation has no default workload id")
		return
	}

	opts := CreateOptions{
		WorkloadID:   workloadID,
		Trigger:      CheckpointTrigger(req.Trigger),
		Mode:         CheckpointMode(req.Mode),
		LeaveRunning: req.LeaveRunning,
		Reason:       req.Reason,
	}
	// Surfaces an unknown mode or trigger as a 400 before any CRIU work starts.
	if err := opts.applyDefaults(); err != nil {
		respondErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	result, err := svc.CreateCheckpoint(r.Context(), target, opts)
	if err != nil {
		respondServiceError(w, err, http.StatusInternalServerError)
		return
	}

	manifest, err := svc.GetCheckpoint(result.CheckpointID)
	if err != nil {
		respondErrorJSON(w, http.StatusInternalServerError, "checkpoint succeeded but its manifest could not be read: "+err.Error())
		return
	}
	utils.RespondJSON(w, http.StatusCreated, checkpointResponse(manifest))
}

// ListCheckpointsHandler handles GET /checkpoints, newest first, across every
// workload in the checkpoint root.
func ListCheckpointsHandler(w http.ResponseWriter, r *http.Request) {
	svc, ok := activeService(w)
	if !ok {
		return
	}

	manifests, err := svc.ListCheckpoints()
	if err != nil {
		respondErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
	})

	responses := make([]CheckpointResponse, len(manifests))
	for i, m := range manifests {
		responses[i] = checkpointResponse(m)
	}
	utils.RespondJSON(w, http.StatusOK, responses)
}

// GetCheckpointHandler handles GET /checkpoints/{id}. A checkpoint in any
// state is returned; the State field says whether it is usable.
func GetCheckpointHandler(w http.ResponseWriter, r *http.Request) {
	svc, ok := activeService(w)
	if !ok {
		return
	}

	id := mux.Vars(r)["id"]
	if err := ValidateCheckpointID(id); err != nil {
		respondErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	manifest, err := svc.GetCheckpoint(id)
	if err != nil {
		respondServiceError(w, err, http.StatusNotFound)
		return
	}
	utils.RespondJSON(w, http.StatusOK, checkpointResponse(manifest))
}

// RestoreCheckpointHandler handles POST /checkpoints/{id}/restore. It returns
// once the restore is confirmed and the application is registered, not when
// the application finishes.
func RestoreCheckpointHandler(w http.ResponseWriter, r *http.Request) {
	svc, ok := activeService(w)
	if !ok {
		return
	}

	id := mux.Vars(r)["id"]
	if err := ValidateCheckpointID(id); err != nil {
		respondErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	// An empty body is a valid restore: every prerequisite falls back to the
	// flags this allocation was started with.
	var req CheckpointRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		respondErrorJSON(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	opts := svc.RestoreDefaults()
	if req.Force != nil {
		opts.Force = *req.Force
	}
	if req.ShutdownOnCompletion != nil {
		opts.ShutdownOnCompletion = *req.ShutdownOnCompletion
	}
	if req.PreRestoreCommands != nil {
		opts.PreRestoreCommands = req.PreRestoreCommands
	}
	if req.EnsureDirs != nil {
		opts.EnsureDirs = req.EnsureDirs
	}
	if req.RequireFiles != nil {
		opts.RequireFiles = req.RequireFiles
	}

	result, err := svc.RestoreCheckpoint(r.Context(), id, opts)
	if err != nil {
		respondServiceError(w, err, http.StatusInternalServerError)
		return
	}

	// The restored workload keeps the identity recorded in the checkpoint, so
	// later checkpoints from this allocation group under it.
	svc.SetDefaultWorkloadID(result.WorkloadID)

	utils.RespondJSON(w, http.StatusOK, CheckpointRestoreResponse{
		CheckpointID: result.CheckpointID,
		WorkloadID:   result.WorkloadID,
		ProcessID:    result.ProcessID,
		PID:          result.Pid,
		RestoredAt:   result.FinishedAt.UTC(),
		Warnings:     result.Warnings,
	})
}
