// Package filesystem will let you mount datasets, copy files and manage the job's files. Nothing is served yet.
//
//	Router    No route yet: adding one is a contract that needs a client (docs/COMPATIBILITY.md).
//	Commands  None yet.
package filesystem

import "github.com/cyber-shuttle/linkspan/internal/router"

var Router = router.New(router.Router{Prefix: "/filesystem"})

var Commands = map[string]router.Command{}
