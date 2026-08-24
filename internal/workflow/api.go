package workflow

import (
	"net/http"

	"github.com/cyber-shuttle/linkspan/internal/utils"
)

/*
GlobalEngine is the engine /status reports on.

main.go installs it when --workflow is given. It stays nil otherwise, and the
endpoint then reports an idle workflow rather than 404ing: a client polling
for progress should get a well-formed answer whether or not this allocation
was started with a workflow.
*/
var GlobalEngine *Engine

// StatusHandler handles GET /status, reporting how far the workflow has got.
func StatusHandler(w http.ResponseWriter, r *http.Request) {
	engine := GlobalEngine
	if engine == nil {
		utils.RespondJSON(w, http.StatusOK, Status{State: StateIdle, Outputs: map[string]any{}})
		return
	}
	utils.RespondJSON(w, http.StatusOK, engine.Status())
}
