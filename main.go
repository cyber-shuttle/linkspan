// Package main parses the flags, serves the HTTP API on loopback and on the optional unix socket, and starts the
// tunnel and the workflow's triggers, all under tasks: one StopAll ends everything and the first fatal error
// reaches main.
//
//	version                 Set by the linker; "dev" otherwise.
//	Config                  Which subsystems publish their routes, by name; main passes a literal until a file
//	                        loader does.
//	subsystem, subsystems   The one table of what each subsystem offers.
//	options, registerFlags  Flag spellings are frozen by docs/COMPATIBILITY.md; a test passes its own FlagSet.
//	routes                  The tree: /api/v1 with health and metrics, then only enabled subsystems.
//	commands                The workflow's actions, each prefixed by its subsystem, from enabled subsystems only.
//	startAll                Validates every input before binding anything, then starts every task in one pass, the
//	                        listeners first: each as h-<port> or h-<socket path>, metrics, the tunnel and the
//	                        workflow by kind, and the workflow's signal tasks as workflow-<signal>.
//	main                    os.Exit is the first defer, so StopAll runs before it; the workflow's stop steps run
//	                        first, with the API still up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/metrics"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/filesystem"
	"github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	"github.com/cyber-shuttle/linkspan/subsystems/terminal"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/cyber-shuttle/linkspan/subsystems/workflow"
)

var version = "dev"

type Config map[string]bool

type subsystem struct {
	router   *router.Router
	commands map[string]router.Command
}

var subsystems = map[string]subsystem{
	"vscode":     {vscode.Router, vscode.Commands},
	"jupyter":    {jupyter.Router, jupyter.Commands},
	"terminal":   {terminal.Router, terminal.Commands},
	"filesystem": {filesystem.Router, filesystem.Commands},
}

type options struct {
	printVersion    bool
	tunnelEnable    bool
	tunnelID        string
	tunnelCluster   string
	tunnelHostToken string
	port            int
	socket          string
	workflow        string
}

func registerFlags(fs *flag.FlagSet) *options {
	var o options
	fs.BoolVar(&o.printVersion, "version", false, "print the version and exit")
	fs.BoolVar(&o.tunnelEnable, "tunnel-enable", false, "host the tunnel named by --tunnel-id")
	fs.StringVar(&o.tunnelID, "tunnel-id", "", "id of the client-created tunnel to host")
	fs.StringVar(&o.tunnelCluster, "tunnel-cluster", "", "cluster id of --tunnel-id")
	fs.StringVar(&o.tunnelHostToken, "tunnel-host-token", "", "host-scoped access token for --tunnel-id")
	fs.IntVar(&o.port, "port", 8080, "loopback port for the HTTP API; 0 picks a free one")
	fs.StringVar(&o.socket, "socket", "", "also serve on this unix socket path")
	fs.StringVar(&o.workflow, "workflow", "", "workflow YAML file")
	return &o
}

func routes(cfg Config) *router.Router {
	root := router.New(router.Router{Prefix: "/api/v1", Routes: map[string]router.Command{
		"GET /health": func(context.Context, map[string]any) (int, any, string) {
			return http.StatusOK, map[string]string{"status": "ok"}, ""
		},
		"GET /metrics": func(context.Context, map[string]any) (int, any, string) { return http.StatusOK, metrics.Latest(), "" },
	}})
	for name, sub := range subsystems {
		if cfg[name] {
			root.Mount(sub.router)
		}
	}
	return root
}

func commands(cfg Config) map[string]router.Command {
	out := map[string]router.Command{}
	for name, sub := range subsystems {
		for command, c := range sub.commands {
			if cfg[name] {
				out[name+"."+command] = c
			}
		}
	}
	return out
}

func startAll(opts *options, cfg Config) error {
	var (
		tn  *tunnel.Tunnel
		err error
	)
	if opts.tunnelEnable {
		if tn, err = tunnel.New(opts.tunnelID, opts.tunnelCluster, opts.tunnelHostToken); err != nil {
			return err
		}
	}
	if opts.workflow != "" {
		if err := workflow.Load(opts.workflow, commands(cfg)); err != nil {
			return err
		}
	}
	addrs := []string{fmt.Sprintf("127.0.0.1:%d", opts.port)}
	if opts.socket != "" {
		addrs = append(addrs, opts.socket)
	}
	h := routes(cfg).Handler()
	var all []*tasks.Task
	for _, addr := range addrs {
		all = append(all, &tasks.Task{Kind: "http", Addr: addr, Server: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}})
	}
	all = append(all, &tasks.Task{Kind: "metrics", Run: metrics.Poll})
	if tn != nil {
		all = append(all, &tasks.Task{Kind: "tunnel", Run: tn.Relay})
	}
	if opts.workflow != "" {
		all = append(all, &tasks.Task{Kind: "workflow", Run: workflow.Start})
		for _, name := range workflow.Signals() {
			all = append(all, &tasks.Task{ID: "workflow-" + name, Kind: "workflow", Run: workflow.WatchSignal(name)})
		}
	}
	for _, t := range all {
		created, err := t.Start()
		if err != nil {
			return err
		}
		if t.Kind == "http" {
			log.Printf("api: listening on %s", created.Addr)
		}
	}
	return nil
}

func main() {
	code := 0
	defer func() { os.Exit(code) }()

	opts := registerFlags(flag.CommandLine)
	flag.Parse()

	if opts.printVersion {
		fmt.Println(version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer func() {
		tasks.StopAll()
		stop()
		log.Println("stopped")
	}()

	if err := startAll(opts, Config{"vscode": true, "jupyter": true, "terminal": false, "filesystem": false}); err != nil {
		log.Printf("fatal: %v", err)
		code = 1
		return
	}

	select {
	case <-ctx.Done():
		log.Println("signal received, stopping")
	case err := <-tasks.Failed:
		log.Printf("fatal: %v", err)
		code = 1
	}
	if err := workflow.Run(context.Background(), "stop"); err != nil {
		log.Printf("fatal: workflow: %v", err)
		code = 1
	}
}
