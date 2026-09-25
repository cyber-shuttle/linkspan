// Package main is the entry point. It parses the flags, serves the HTTP API on a loopback port, a unix socket or both,
// and starts the tunnel modes and workflow triggers, all as tasks, so one StopAll ends everything and the first fatal
// error reaches main.
//
//	version                 Set by the linker; "dev" otherwise.
//	subsystem, subsystems   The one table of what each subsystem offers.
//	options, registerFlags  Flag spellings are frozen by docs/COMPATIBILITY.md; a test passes its own FlagSet. portSet
//	                        is whether --port was given, since --socket alone drops the default port.
//	routes                  The tree: /api/v1 with health and metrics, then only enabled subsystems.
//	commands                The workflow's actions: its own unprefixed, and each enabled subsystem's behind its name.
//	startAll                Validates every input before binding anything, then starts every task in one pass, the
//	                        listeners first as h-<port> and h-<socket path>, then metrics, each tunnel mode and the
//	                        workflow by kind.
//	main                    os.Exit is the first defer, so StopAll runs before it; the workflow's stop steps run
//	                        first, with the API still up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cyber-shuttle/linkspan/internal/forward"
	"github.com/cyber-shuttle/linkspan/internal/metrics"
	"github.com/cyber-shuttle/linkspan/internal/router"
	"github.com/cyber-shuttle/linkspan/internal/tasks"
	"github.com/cyber-shuttle/linkspan/internal/tunnel"
	"github.com/cyber-shuttle/linkspan/subsystems/checkpoint"
	"github.com/cyber-shuttle/linkspan/subsystems/filesystem"
	"github.com/cyber-shuttle/linkspan/subsystems/jupyter"
	"github.com/cyber-shuttle/linkspan/subsystems/terminal"
	"github.com/cyber-shuttle/linkspan/subsystems/vscode"
	"github.com/cyber-shuttle/linkspan/subsystems/workflow"
)

var version = "dev"

type subsystem struct {
	router   *router.Router
	commands map[string]router.Command
}

var subsystems = map[string]subsystem{
	"workflow":   {workflow.Router, nil},
	"vscode":     {vscode.Router, vscode.Commands},
	"jupyter":    {jupyter.Router, jupyter.Commands},
	"terminal":   {terminal.Router, terminal.Commands},
	"filesystem": {filesystem.Router, filesystem.Commands},
	"checkpoint": {checkpoint.Router, checkpoint.Commands},
}

type options struct {
	printVersion bool
	tunnelEnable bool
	tunnelMode   string
	tunnelArgs   map[string]string
	port         int
	portSet      bool
	socket       string
	workflow     string
}

func registerFlags(fs *flag.FlagSet) *options {
	o := options{tunnelArgs: map[string]string{}}
	fs.BoolVar(&o.printVersion, "version", false, "print the version and exit")
	fs.BoolVar(&o.tunnelEnable, "tunnel-enable", false, "carry the API off the node in the modes of --tunnel-mode")
	fs.StringVar(&o.tunnelMode, "tunnel-mode", "", "comma-separated `modes`, websocket and/or devtunnel; required with --tunnel-enable")
	for name, m := range tunnel.Modes {
		fs.Func("tunnel-"+name+"-args", m.Usage, func(v string) error { o.tunnelArgs[name] = v; return nil })
	}
	fs.IntVar(&o.port, "port", 8080, "HTTP API port on loopback; 0 picks a free one")
	fs.StringVar(&o.socket, "socket", "", "HTTP API unix socket `path`, owner-only; alone, it replaces the port")
	fs.StringVar(&o.workflow, "workflow", "", "workflow YAML file")
	return &o
}

func routes() *router.Router {
	root := router.New("/api/v1", map[string]router.Command{
		"GET /health": func(context.Context, map[string]any) (int, any, string) {
			return http.StatusOK, map[string]string{"status": "ok"}, ""
		},
		"GET /metrics": func(context.Context, map[string]any) (int, any, string) { return http.StatusOK, metrics.Latest(), "" },
	})
	for _, sub := range subsystems {
		root.Mount(sub.router)
	}
	return root
}

func commands() map[string]router.Command {
	out := maps.Clone(workflow.Commands)
	for name, sub := range subsystems {
		for command, c := range sub.commands {
			out[name+"."+command] = c
		}
	}
	return out
}

func startAll(opts *options) error {
	var addrs []string
	if opts.portSet || opts.socket == "" {
		addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", opts.port))
	}
	if opts.socket != "" {
		addrs = append(addrs, opts.socket)
	}
	tunnels, err := tunnel.Parse(opts.tunnelEnable, opts.tunnelMode, opts.tunnelArgs)
	if err != nil {
		return err
	}
	if opts.workflow != "" {
		if err := workflow.Load(opts.workflow, commands()); err != nil {
			return err
		}
	}
	h := http.NewServeMux()
	h.Handle("/", routes().Handler())
	h.HandleFunc("GET /api/v1/forward/{port}", forward.Stream)
	var all []*tasks.Task
	for _, addr := range addrs {
		all = append(all, &tasks.Task{Kind: "http", Addr: addr, Server: &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}})
	}
	all = append(append(all, &tasks.Task{Kind: "metrics", Run: metrics.Poll}), tunnels...)
	if opts.workflow != "" {
		all = append(all, &tasks.Task{Kind: "workflow", Run: workflow.Start()})
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
	flag.Visit(func(f *flag.Flag) { opts.portSet = opts.portSet || f.Name == "port" })

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

	if err := startAll(opts); err != nil {
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
