package vfs

import (
	"net/http"

	utils "github.com/cyber-shuttle/conduit/utils"
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
