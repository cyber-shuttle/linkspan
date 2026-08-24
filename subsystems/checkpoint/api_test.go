package checkpoint

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	pm "github.com/cyber-shuttle/linkspan/internal/process"
	"github.com/gorilla/mux"
)

// installTestService points the handlers at svc for one test. The handlers
// read a package global, mirroring how every other subsystem exposes its
// manager, so the swap has to be undone afterwards.
func installTestService(t *testing.T, svc *CheckpointService) {
	t.Helper()
	orig := GlobalCheckpointService
	GlobalCheckpointService = svc
	t.Cleanup(func() { GlobalCheckpointService = orig })
}

// newTestRouter wires the same routes api.go registers, so {id} extraction and
// method matching are exercised rather than the handlers being called directly.
func newTestRouter() *mux.Router {
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/checkpoints", CreateCheckpointHandler).Methods("POST")
	api.HandleFunc("/checkpoints", ListCheckpointsHandler).Methods("GET")
	api.HandleFunc("/checkpoints/{id}/restore", RestoreCheckpointHandler).Methods("POST")
	api.HandleFunc("/checkpoints/{id}", GetCheckpointHandler).Methods("GET")
	return r
}

func doRequest(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rr := httptest.NewRecorder()
	newTestRouter().ServeHTTP(rr, req)
	return rr
}

func decodeCheckpoint(t *testing.T, rr *httptest.ResponseRecorder) CheckpointResponse {
	t.Helper()
	var resp CheckpointResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode checkpoint response %q: %v", rr.Body.String(), err)
	}
	return resp
}

func TestLeaveRunningDefaultsByTrigger(t *testing.T) {
	manual := CreateOptions{WorkloadID: "wl"}
	if err := manual.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if !manual.leaveRunning() {
		t.Fatalf("a manual checkpoint must leave the process running by default")
	}
	if manual.Trigger != TriggerManual || manual.Mode != ModeAuto {
		t.Fatalf("expected manual/auto defaults, got %q/%q", manual.Trigger, manual.Mode)
	}

	walltime := CreateOptions{WorkloadID: "wl", Trigger: TriggerWalltime}
	if err := walltime.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if walltime.leaveRunning() {
		t.Fatalf("a walltime checkpoint must stop the process by default")
	}

	// An explicit value always wins over the trigger's default.
	keep := true
	explicit := CreateOptions{WorkloadID: "wl", Trigger: TriggerWalltime, LeaveRunning: &keep}
	if err := explicit.applyDefaults(); err != nil {
		t.Fatalf("applyDefaults failed: %v", err)
	}
	if !explicit.leaveRunning() {
		t.Fatalf("an explicit leave_running must override the trigger default")
	}
}

func TestCreateOptionsRejectsUnknownMode(t *testing.T) {
	opts := CreateOptions{WorkloadID: "wl", Mode: CheckpointMode("tpu")}
	if err := opts.applyDefaults(); err == nil {
		t.Fatalf("expected an unknown checkpoint mode to be rejected")
	}
	if err := (&CreateOptions{}).applyDefaults(); err == nil {
		t.Fatalf("expected a missing workload id to be rejected")
	}
}

func TestValidateCheckpointID(t *testing.T) {
	if err := ValidateCheckpointID("ckpt-20260823T101500Z-a1b2c3d4"); err != nil {
		t.Fatalf("expected a generated checkpoint id to be valid: %v", err)
	}
	for _, bad := range []string{"", "..", "../../etc", "ckpt-*", "wl/ckpt", "ckpt?1", "ckpt[12]"} {
		if err := ValidateCheckpointID(bad); err == nil {
			t.Fatalf("expected checkpoint id %q to be rejected", bad)
		}
	}
}

func TestCheckpointEndpointsReportUnconfiguredService(t *testing.T) {
	// A service with no CRIU path and no checkpoint root is what an allocation
	// started without checkpoint flags has.
	installTestService(t, &CheckpointService{criu: &criuCheckpointer{}, workloads: map[string]*workloadEntry{}})

	for _, call := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/checkpoints"},
		{http.MethodPost, "/api/v1/checkpoints"},
		{http.MethodGet, "/api/v1/checkpoints/ckpt-1"},
		{http.MethodPost, "/api/v1/checkpoints/ckpt-1/restore"},
	} {
		rr := doRequest(t, call.method, call.path, "{}")
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: expected 503 without checkpoint configuration, got %d", call.method, call.path, rr.Code)
		}
	}
}

func TestCreateCheckpointRequiresExactlyOneTarget(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	if rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"workload_id":"wl-a"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when neither process_id nor pid is given, got %d", rr.Code)
	}
	if rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"workload_id":"wl-a","pid":123,"process_id":"p-1"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when both process_id and pid are given, got %d", rr.Code)
	}
}

func TestCreateCheckpointRejectsUnknownModeOverREST(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"workload_id":"wl-a","pid":1,"mode":"tpu"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown mode, got %d: %s", rr.Code, rr.Body.String())
	}
}

// An allocation with no workload id of its own cannot invent one for a request
// that names none, so the request is refused rather than guessed at.
func TestCreateCheckpointRequiresAWorkloadIDSomewhere(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	if rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"pid":1}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without a workload id anywhere, got %d", rr.Code)
	}
}

func TestGetCheckpointRejectsMalformedID(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	rr := doRequest(t, http.MethodGet, "/api/v1/checkpoints/ckpt%2A", "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a glob metacharacter in the id, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestGetCheckpointUnknownIDIsNotFound(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	rr := doRequest(t, http.MethodGet, "/api/v1/checkpoints/ckpt-does-not-exist", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an unknown checkpoint, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestRestoreUnknownCheckpointIsNotFound(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints/ckpt-does-not-exist/restore", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 restoring an unknown checkpoint, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestListCheckpointsEmptyRoot(t *testing.T) {
	installTestService(t, newTestService(t, "/nonexistent/criu", ""))

	rr := doRequest(t, http.MethodGet, "/api/v1/checkpoints", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 listing an empty checkpoint root, got %d", rr.Code)
	}
	var list []CheckpointResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("expected a JSON array, got %q: %v", rr.Body.String(), err)
	}
	if len(list) != 0 {
		t.Fatalf("expected no checkpoints, got %d", len(list))
	}
}

// The deliverable: checkpoint a real process over HTTP with the manual
// default, and find it still running afterwards. The body names no workload,
// so this also covers the fallback to the allocation's own.
func TestCreateCheckpointOverRESTLeavesProcessRunning(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	svc.SetDefaultWorkloadID("wl-rest")
	installTestService(t, svc)

	pid := startDetachedProcess(t, "600")
	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"pid":`+strconv.Itoa(pid)+`,"reason":"stage 5 smoke test"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeCheckpoint(t, rr)
	if resp.State != StateComplete {
		t.Fatalf("expected a complete checkpoint, got state %q", resp.State)
	}
	if !resp.LeaveRunning {
		t.Fatalf("a manual checkpoint must report leave_running=true, got false")
	}
	if resp.Reason != "stage 5 smoke test" {
		t.Fatalf("expected the reason to be recorded, got %q", resp.Reason)
	}
	if resp.WorkloadID != "wl-rest" || resp.PID != pid {
		t.Fatalf("expected wl-rest/%d, got %s/%d", pid, resp.WorkloadID, resp.PID)
	}

	if err := checkPidExists(pid); err != nil {
		t.Fatalf("--leave-running must keep pid %d alive after the dump: %v", pid, err)
	}
	// Still running means still checkpointable — the state machine must not
	// have parked the workload in "checkpointed".
	if got := svc.WorkloadState("wl-rest"); got != WorkloadRunning {
		t.Fatalf("expected workload state running after a leave-running checkpoint, got %q", got)
	}
}

// A left-running workload must accept a second checkpoint, which is what makes
// periodic checkpointing possible at all.
func TestSecondCheckpointOfLeftRunningWorkload(t *testing.T) {
	installTestService(t, newTestService(t, requireCriu(t), ""))

	pid := startDetachedProcess(t, "600")
	body := `{"workload_id":"wl-twice","pid":` + strconv.Itoa(pid) + `}`

	first := doRequest(t, http.MethodPost, "/api/v1/checkpoints", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first checkpoint failed with %d: %s", first.Code, first.Body.String())
	}
	second := doRequest(t, http.MethodPost, "/api/v1/checkpoints", body)
	if second.Code != http.StatusCreated {
		t.Fatalf("second checkpoint of a left-running workload failed with %d: %s", second.Code, second.Body.String())
	}

	if decodeCheckpoint(t, first).CheckpointID == decodeCheckpoint(t, second).CheckpointID {
		t.Fatalf("expected the two checkpoints to have distinct ids")
	}
}

func TestCreateCheckpointStopsProcessWhenAsked(t *testing.T) {
	installTestService(t, newTestService(t, requireCriu(t), ""))

	pid := startDetachedProcess(t, "600")
	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"workload_id":"wl-stop","pid":`+strconv.Itoa(pid)+`,"leave_running":false}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if decodeCheckpoint(t, rr).LeaveRunning {
		t.Fatalf("expected leave_running=false to be recorded")
	}
	if err := checkPidExists(pid); err == nil {
		t.Fatalf("expected pid %d to be stopped by the dump", pid)
	}
}

// A walltime checkpoint takes the opposite default without being told.
func TestWalltimeCheckpointStopsByDefaultOverREST(t *testing.T) {
	installTestService(t, newTestService(t, requireCriu(t), ""))

	pid := startDetachedProcess(t, "600")
	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints", `{"workload_id":"wl-walltime","pid":`+strconv.Itoa(pid)+`,"trigger":"walltime"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}

	resp := decodeCheckpoint(t, rr)
	if resp.LeaveRunning {
		t.Fatalf("a walltime checkpoint must default to stopping the process")
	}
	if resp.Trigger != TriggerWalltime {
		t.Fatalf("expected the walltime trigger to be recorded, got %q", resp.Trigger)
	}
	if err := checkPidExists(pid); err == nil {
		t.Fatalf("expected pid %d to be stopped by a walltime dump", pid)
	}
}

// The full stage 5 deliverable: checkpoint and restore an application over
// REST alone, and get back a handle on the restored application.
func TestCheckpointAndRestoreOverREST(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	installTestService(t, svc)

	pid := startDetachedProcess(t, "600")
	created := doRequest(t, http.MethodPost, "/api/v1/checkpoints",
		`{"workload_id":"wl-roundtrip","pid":`+strconv.Itoa(pid)+`,"leave_running":false}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("checkpoint failed with %d: %s", created.Code, created.Body.String())
	}
	checkpointID := decodeCheckpoint(t, created).CheckpointID

	got := doRequest(t, http.MethodGet, "/api/v1/checkpoints/"+checkpointID, "")
	if got.Code != http.StatusOK {
		t.Fatalf("GET checkpoint failed with %d: %s", got.Code, got.Body.String())
	}
	if decodeCheckpoint(t, got).CheckpointID != checkpointID {
		t.Fatalf("GET returned the wrong checkpoint")
	}

	listed := doRequest(t, http.MethodGet, "/api/v1/checkpoints", "")
	var list []CheckpointResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to decode list: %v", err)
	}
	if len(list) != 1 || list[0].CheckpointID != checkpointID {
		t.Fatalf("expected the new checkpoint in the listing, got %+v", list)
	}

	restored := doRequest(t, http.MethodPost, "/api/v1/checkpoints/"+checkpointID+"/restore", "")
	if restored.Code != http.StatusOK {
		t.Fatalf("restore failed with %d: %s", restored.Code, restored.Body.String())
	}

	var resp CheckpointRestoreResponse
	if err := json.Unmarshal(restored.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode restore response %q: %v", restored.Body.String(), err)
	}
	defer pm.GlobalProcessManager.Kill(resp.ProcessID)

	if resp.WorkloadID != "wl-roundtrip" {
		t.Fatalf("expected the workload id to be resolved from disk, got %q", resp.WorkloadID)
	}
	if resp.ProcessID == "" || resp.PID <= 0 {
		t.Fatalf("expected the restored application's identity, got %+v", resp)
	}
	if err := checkPidExists(resp.PID); err != nil {
		t.Fatalf("restored pid %d is not running: %v", resp.PID, err)
	}
	// The restored workload's identity becomes this allocation's default, so
	// a later checkpoint over REST groups under it.
	if got := svc.DefaultWorkloadID(); got != "wl-roundtrip" {
		t.Fatalf("expected the restored workload to become the default, got %q", got)
	}
}

// A restore body may override the flags the allocation was started with.
func TestRestoreOverRESTHonoursRequestOverrides(t *testing.T) {
	svc := newTestService(t, requireCriu(t), "")
	installTestService(t, svc)

	checkpointID := checkpointRealProcess(t, svc, "wl-override")
	dir := checkpointDirPath(svc.criu.CheckpointRoot, "wl-override", checkpointID)

	staged := t.TempDir() + "/staged-app"
	rewriteManifest(t, dir, func(m *Manifest) { m.Executable = staged })

	rr := doRequest(t, http.MethodPost, "/api/v1/checkpoints/"+checkpointID+"/restore",
		`{"pre_restore_commands":["cp /bin/sleep `+staged+`"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected the staged executable to satisfy the restore, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp CheckpointRestoreResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode restore response: %v", err)
	}
	defer pm.GlobalProcessManager.Kill(resp.ProcessID)
}
