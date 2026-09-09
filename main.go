// Package main parses the flags, binds the HTTP API on loopback, and starts
// the workflow and the tunnel. Everything runs under procmgr, so one StopAll
// ends it all and the first fatal error reaches main on one channel.
//
//	version        It is set by the linker, and is "dev" otherwise.
//	options        Its flag spellings are frozen by docs/COMPATIBILITY.md.
//	registerFlags  It takes a FlagSet so a test can use a fresh one.
//	startAll       It validates every input before binding anything.
//	main           os.Exit is the first defer, so StopAll runs before it.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cyber-shuttle/linkspan/internal/httpapi"
	"github.com/cyber-shuttle/linkspan/internal/procmgr"
	"github.com/cyber-shuttle/linkspan/internal/workflow"
	"github.com/cyber-shuttle/linkspan/subsystems/tunnel"
)

var version = "dev"

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

func startAll(opts *options) error {
	var (
		tn   *tunnel.Tunnel
		wf   *workflow.Workflow
		sock *httpapi.HttpAPI
		err  error
	)
	if opts.tunnelEnable {
		if tn, err = tunnel.New(opts.tunnelID, opts.tunnelCluster, opts.tunnelHostToken); err != nil {
			return err
		}
	}
	if opts.workflow != "" {
		if wf, err = workflow.New(opts.workflow); err != nil {
			return err
		}
	}

	tcp, err := httpapi.New("tcp", fmt.Sprintf("127.0.0.1:%d", opts.port))
	if err != nil {
		return err
	}
	if opts.socket != "" {
		if sock, err = httpapi.New("unix", opts.socket); err != nil {
			return err
		}
	}
	tcp.Start()
	if sock != nil {
		sock.Start()
	}
	if wf != nil {
		wf.Start()
	}
	if tn != nil {
		tn.Start()
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
		procmgr.StopAll()
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
	case err := <-procmgr.Failed:
		log.Printf("fatal: %v", err)
		code = 1
	}
}
