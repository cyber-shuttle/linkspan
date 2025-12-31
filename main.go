package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	jupyter "github.com/cyber-shuttle/conduit/subsystems/jupyter"
	tunnel "github.com/cyber-shuttle/conduit/subsystems/tunnel"
	vfs "github.com/cyber-shuttle/conduit/subsystems/vfs"
	vscode "github.com/cyber-shuttle/conduit/subsystems/vscode"
	"github.com/gorilla/mux"
	pm "github.com/cyber-shuttle/conduit/internal/process"
)

func main() {

	ctx, stop := signal.NotifyContext(context.Background(), 
		os.Interrupt, // Ctrl+C 
		syscall.SIGTERM, // termination (reliable on Linux/macOS) 
	) 
	defer stop() 

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()

	// Jupyter kernel management
	api.HandleFunc("/jupyter/kernels", jupyter.ListKernels).Methods("GET")
	api.HandleFunc("/jupyter/kernels", jupyter.ProvisionKernel).Methods("POST")
	api.HandleFunc("/jupyter/kernels/{id}", jupyter.DeleteKernel).Methods("DELETE")
	api.HandleFunc("/jupyter/kernels/{id}/connection", jupyter.GetKernelConnectionInfo).Methods("GET")
	api.HandleFunc("/jupyter/kernels/{id}/status", jupyter.GetKernelStatus).Methods("GET")
	api.HandleFunc("/jupyter/kernels/shutdown", jupyter.ShutdownKernel).Methods("POST")

	// VS Code remote session management
	api.HandleFunc("/vscode/sessions", vscode.ListVSCodes).Methods("GET")
	api.HandleFunc("/vscode/sessions", vscode.CreateVSCodeSession).Methods("POST")
	api.HandleFunc("/vscode/sessions/{id}", vscode.TerminateVSCodeSession).Methods("DELETE")

	// Remote filesystem management
	api.HandleFunc("/fs/list", vfs.ListFiles).Methods("GET")
	api.HandleFunc("/fs/read", vfs.ReadFile).Methods("GET")
	api.HandleFunc("/fs/write", vfs.WriteFile).Methods("POST")
	api.HandleFunc("/fs/delete", vfs.DeleteFile).Methods("DELETE")

	// Tunnel management
	api.HandleFunc("/tunnels", tunnel.ListTunnels).Methods("GET")
	api.HandleFunc("/tunnels", tunnel.CreateTunnel).Methods("POST")
	api.HandleFunc("/tunnels/{id}", tunnel.CloseTunnel).Methods("DELETE")

	addr := ":8080"

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// Run server in a goroutine so main can wait for shutdown signal
	serverErr := make(chan error, 1)

	go func() {
		log.Printf("starting server on %s", addr)
		err := srv.ListenAndServe()
		serverErr <- err
	}()

	select {
		case <-ctx.Done():
			// Ctrl+C / SIGTERM received
			log.Println("Shutdown signal received...")
		case err := <-serverErr:
			// Server failed to start or died unexpectedly
			if err != nil && err != http.ErrServerClosed {
				log.Printf("server error: %v", err)
			}
			return
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10* time.Second)
	defer cancel()

	// Stop accepting new connections + wait for in-flight requests
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server shutdown error: %v", err)
	}

	cleanupResources()

	log.Println("Server gracefully stopped.")
}

func cleanupResources() {
	log.Println("Cleaning up resources before shutdown...")
	// Add resource cleanup logic here, e.g., terminating kernels, closing tunnels, etc.
	pm.GlobalProcessManager.KillAll()
}
