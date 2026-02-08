package vfs

import (
	"encoding/json"
	"net/http"

	utils "github.com/cyber-shuttle/linkspan/utils"
	"github.com/gorilla/mux"
)

type FileInfo struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

func ListFiles(w http.ResponseWriter, r *http.Request) {
	files := []FileInfo{{Path: "/home/alice/notebook.ipynb", Size: 12345, IsDir: false}}
	utils.RespondJSON(w, http.StatusOK, files)
}

func ReadFile(w http.ResponseWriter, r *http.Request) {
	// placeholder: read file content
	content := map[string]string{"path": "/home/alice/notebook.ipynb", "content": "# notebook"}
	utils.RespondJSON(w, http.StatusOK, content)
}

func WriteFile(w http.ResponseWriter, r *http.Request) {
	// placeholder: write file
	utils.RespondJSON(w, http.StatusCreated, map[string]string{"result": "ok"})
}

func DeleteFile(w http.ResponseWriter, r *http.Request) {
	// placeholder: delete file
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// ListMounts returns all active FUSE mounts.
func ListMounts(w http.ResponseWriter, r *http.Request) {
	list := GlobalMountManager.List()
	utils.RespondJSON(w, http.StatusOK, list)
}

// CreateMount mounts a remote FS at the given mountpoint (POST body: MountConfig JSON).
func CreateMount(w http.ResponseWriter, r *http.Request) {
	var cfg MountConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if cfg.Mountpoint == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "mountpoint required"})
		return
	}
	if cfg.ServerAddr == "" && (cfg.Token == "" || cfg.FRPConnection == "") {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "server_addr or (token and frp_connection) required"})
		return
	}
	if cfg.Token != "" && cfg.FRPConnection == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "frp_connection required when using token"})
		return
	}
	id, err := GlobalMountManager.Mount(cfg)
	if err != nil {
		if err == ErrUnsupported {
			utils.RespondJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// UnmountMount unmounts by ID.
func UnmountMount(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	err := GlobalMountManager.Unmount(id)
	if err != nil {
		if err == ErrMountNotFound {
			utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}

// ListPublishes returns all active gRPC publish servers.
func ListPublishes(w http.ResponseWriter, r *http.Request) {
	list := GlobalPublishManager.ListPublishes()
	utils.RespondJSON(w, http.StatusOK, list)
}

// CreatePublish starts publishing a folder (POST body: PublishConfig JSON).
// When frp_connection is set, response includes "token" (id:secret) for the mount client.
func CreatePublish(w http.ResponseWriter, r *http.Request) {
	var cfg PublishConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if cfg.Folder == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "folder required"})
		return
	}
	result, err := GlobalPublishManager.Publish(cfg)
	if err != nil {
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]string{"id": result.ID}
	if result.Token != "" {
		resp["token"] = result.Token
	}
	utils.RespondJSON(w, http.StatusCreated, resp)
}

// StopPublish stops a publish server by ID.
func StopPublish(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if id == "" {
		utils.RespondJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	err := GlobalPublishManager.Stop(id)
	if err != nil {
		if err == ErrPublishNotFound {
			utils.RespondJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		utils.RespondJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	utils.RespondJSON(w, http.StatusNoContent, nil)
}
