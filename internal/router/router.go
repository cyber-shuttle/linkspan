// Package router builds the HTTP route table as a tree, and every route is a command. A Router is a prefix and
// its Routes, given as "METHOD path" relative to the prefix and each naming a command. Routes is the whole table:
// New writes each route behind Prefix and Mount copies a child's table behind it again, so a tree is assembled
// leaf first and one function is a route and a workflow action alike. The route decodes the body as the params
// and adds the path id. Requests carry no credential: reaching a listener is the authorisation (SECURITY.md).
//
//	Command  Returns a status, a body and an error message.
//	Router   Routes is held keyed by "METHOD path" behind Prefix.
//	respond  The one reader and writer: the body decoded as the params, an absent one being none, an oversize one
//	         refused with 413 and any other failure with 400; a non-empty message becomes the error shape.
//	add      The one writer of Routes.
//	New      A router at prefix serving the routes given relative to it, the pointer Mount chains on.
//	Mount    Returns the router, so a tree is one expression.
//	Handler  The table as a ServeMux with bodies capped at 64KB.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

type Command func(ctx context.Context, params map[string]any) (status int, body any, errMsg string)

type Router struct {
	Prefix string
	Routes map[string]Command
}

func respond(c Command) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := map[string]any{}
		var status int
		var body any
		var errMsg string
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil && !errors.Is(err, io.EOF) {
			status, errMsg = http.StatusBadRequest, "invalid JSON: "+err.Error()
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				status, errMsg = http.StatusRequestEntityTooLarge, err.Error()
			}
		} else {
			if params == nil {
				params = map[string]any{}
			}
			if id := r.PathValue("id"); id != "" {
				params["id"] = id
			}
			status, body, errMsg = c(r.Context(), params)
		}
		if errMsg != "" {
			body = map[string]string{"error": errMsg}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (r *Router) add(routes map[string]Command) *Router {
	for pattern, c := range routes {
		method, path, _ := strings.Cut(pattern, " ")
		r.Routes[method+" "+r.Prefix+path] = c
	}
	return r
}

func New(prefix string, routes map[string]Command) *Router {
	return (&Router{Prefix: prefix, Routes: map[string]Command{}}).add(routes)
}

func (r *Router) Mount(child *Router) *Router {
	return r.add(child.Routes)
}

func (r *Router) Handler() http.Handler {
	m := http.NewServeMux()
	for pattern, c := range r.Routes {
		m.HandleFunc(pattern, respond(c))
	}
	return http.MaxBytesHandler(m, 64<<10)
}
