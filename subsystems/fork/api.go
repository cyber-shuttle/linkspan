package fork

import (
	"encoding/json"
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/utils"
	"github.com/gorilla/mux"
)

type ForkProcessRequest struct {
	Command              string `json:"command"`
	ShutdownOnCompletion bool   `json:"shutdownOnCompletion"`
}

type ForkProcessResponse struct {
	ProcessId            string `json:"processId"`
	Command              string `json:"command"`
	ShutdownOnCompletion bool   `json:"shutdownOnCompletion"`
}

// RunForkProcessHandler handles POST /fork/run to start a new fork process.
func RunForkProcessHandler(w http.ResponseWriter, r *http.Request) {
	var req ForkProcessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
		return
	}

	if req.Command == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "command is required",
		})
		return
	}

	fp, err := GlobalForkProcessManager.RunForkProcess(req.Command, req.ShutdownOnCompletion)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to run fork process: " + err.Error(),
		})
		return
	}

	resp := ForkProcessResponse{
		ProcessId:            fp.InternalProcessId,
		Command:              fp.Command,
		ShutdownOnCompletion: fp.ShutdownOnCompletion,
	}
	utils.RespondJSON(w, http.StatusCreated, resp)
}

// ListForkProcessesHandler handles GET /fork/processes to list all fork processes.
func ListForkProcessesHandler(w http.ResponseWriter, r *http.Request) {
	procs := GlobalForkProcessManager.ListForkProcesses()
	responses := make([]ForkProcessResponse, len(procs))
	for i, fp := range procs {
		responses[i] = ForkProcessResponse{
			ProcessId:            fp.InternalProcessId,
			Command:              fp.Command,
			ShutdownOnCompletion: fp.ShutdownOnCompletion,
		}
	}
	utils.RespondJSON(w, http.StatusOK, responses)
}

// DeleteForkProcessHandler handles DELETE /fork/processes/{id} to remove/kill a fork process.
func DeleteForkProcessHandler(w http.ResponseWriter, r *http.Request) {
	processId := mux.Vars(r)["id"]
	if processId == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{
			"error": "process id is required",
		})
		return
	}

	err := GlobalForkProcessManager.RemoveForkProcess(processId)
	if err != nil {
		utils.RespondJSON(w, http.StatusNotFound, map[string]string{
			"error": "fork process not found: " + err.Error(),
		})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
