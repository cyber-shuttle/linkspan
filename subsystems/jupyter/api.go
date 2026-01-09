package jupyter

import (
	"encoding/json"
	"net/http"
	"os"

	utils "github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

type Kernel struct {
	Name string `json:"name"`
}

type KernelProvisionRequest struct {
	KernelName string `json:"kernelName"`
	VenvPath   string `json:"venvPath"`
	CondaEnv string `json:"condaEnv"`
}

type KernelProvisionResponse struct {
	KernelID string `json:"kernelId"`
	Status   string `json:"status"`
}

type ConnInfo struct {
	ShellPort       int    `json:"shell_port"`
	IOPubPort       int    `json:"iopub_port"`
	StdinPort       int    `json:"stdin_port"`
	ControlPort     int    `json:"control_port"`
	HBPort          int    `json:"hb_port"`
	IPAddress       string `json:"ip"`
	Key             string `json:"key"`
	Transport       string `json:"transport"`
	SignatureScheme string `json:"signature_scheme"`
	KernelName      string `json:"kernel_name"`
}

type KernelConnectionInfoResponse struct {
	KernelID       string `json:"kernelId"`
	ConnectionInfo ConnInfo `json:"connectionInfo"`
}

type KernelShutdownRequest struct {
	KernelID string `json:"kernelId"`
	Signal  int `json:"signal"`
}

type KernelShutdownResponse struct {
	KernelID string `json:"kernelId"`
	Status   string `json:"status"`
}

type KernelStatusRequest struct {
	KernelID string 
}

type KernelStatusResponse struct {
	KernelID string 
	Status   string 
}

func ListKernels(w http.ResponseWriter, r *http.Request) {
	kernels := []Kernel{
		{Name: "python3"},
	}
	utils.RespondJSON(w, http.StatusOK, kernels)
}

func ProvisionKernel(w http.ResponseWriter, r *http.Request) {
	// placeholder: parse request body to create kernel
	provisionReq := KernelProvisionRequest{}
	_ = json.NewDecoder(r.Body).Decode(&provisionReq)
	_ = r.Body.Close()

	if provisionReq.KernelName == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "Kernel name is required"})
		return
	}

	if provisionReq.VenvPath != "" && provisionReq.CondaEnv != "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "Specify either venvPath or condaEnv, not both"})
		return
	}

	if provisionReq.VenvPath == "" && provisionReq.CondaEnv == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "Specify either venvPath or condaEnv"})
		return
	}

	if provisionReq.VenvPath != "" {
		// start kernel with venv
		kernelID, _, err := startKernelWithVenv(provisionReq.KernelName, provisionReq.VenvPath)
		if err != nil {
			utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		proResp := KernelProvisionResponse{
			KernelID: kernelID,
			Status:   "starting",
		}
		utils.RespondJSON(w, http.StatusCreated, proResp)
		return
	} else {
		// start kernel with conda env
		utils.RespondJSON(w, http.StatusInternalServerError, 
			map[string]string{"error": "Conda environment backed kernel provisioning is not yet implemented"})
		return
	}
}

func ShutdownKernel(w http.ResponseWriter, r *http.Request) {
	// placeholder: shutdown by id
	shutdownReq := KernelShutdownRequest{}
	_ = json.NewDecoder(r.Body).Decode(&shutdownReq)
	_ = r.Body.Close()

	err := stopKernel(shutdownReq.KernelID)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// placeholder: shutdown kernel based on shutdownReq.KernelID
	shutdownResp := KernelShutdownResponse{
		KernelID: shutdownReq.KernelID,
		Status:   "shutting down",
	}
	utils.RespondJSON(w, http.StatusOK, shutdownResp)
}

func DeleteKernel(w http.ResponseWriter, r *http.Request) {
	// placeholder: delete by id
	deleteReq := KernelShutdownRequest{}
	_ = json.NewDecoder(r.Body).Decode(&deleteReq)
	_ = r.Body.Close()

	err := stopKernel(deleteReq.KernelID)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

func GetKernelStatus(w http.ResponseWriter, r *http.Request) {
	// read id from URL path
	vars := mux.Vars(r)
	kernelID := vars["id"]

	if kernelID == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "KernelID is required"})
		return
	}

	status, err := getKernelStatus(kernelID)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// placeholder: get kernel status based on statusReq.KernelID
	statusResp := KernelStatusResponse{
		KernelID: kernelID,
		Status:   status,
	}
	utils.RespondJSON(w, http.StatusOK, statusResp)
}

func GetKernelConnectionInfo(w http.ResponseWriter, r *http.Request) {
	// placeholder: get connection info by id
	vars := mux.Vars(r)
	kernelID := vars["id"]

	if kernelID == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "KernelID is required"})
		return
	}

	filepath, err := getKernelConnectionFile(kernelID)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	data, err := os.ReadFile(filepath)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var connInfo ConnInfo

	err = json.Unmarshal(data, &connInfo)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// placeholder: get kernel connection info based on connInfoReq.KernelID
	connInfoResp := KernelConnectionInfoResponse{
		KernelID:       kernelID,
		ConnectionInfo: connInfo,
	}
	utils.RespondJSON(w, http.StatusOK, connInfoResp)
}