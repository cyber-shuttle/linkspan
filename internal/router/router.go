// Package router is a tree of routers: a Router is a prefix, the commands of the resource at it, and the routes and
// routers mounted under it. Routes is the whole table: New writes every route behind Prefix and Mount copies a child's
// table behind it again, so a tree is built leaf first and Prefix is read only then. A Command answers a params map,
// so one function is a route and a workflow action alike: the route decodes the body as the params and adds the path
// id. Requests carry no credential: reaching a listener is the authorisation (SECURITY.md).
//
//	Command  Returns a status, a body and an error message.
//	Router   Select is GET, Create POST and Stop DELETE /{id} at Prefix, a nil field being no route; Routes is
//	         given as "METHOD path" relative to Prefix and held keyed by the path behind it.
//	decode   The one body reader: an absent body is no params, an oversize one 413, any other failure 400.
//	respond  The only writer; a non-empty message becomes the error shape.
//	add      The one writer of Routes; a nil command is skipped.
//	New      Takes any subset of the fields and returns the pointer Mount chains on.
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
	Prefix               string
	Select, Create, Stop Command
	Routes               map[string]Command
}

func decode(r *http.Request) (map[string]any, int, string) {
	params := map[string]any{}
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil && !errors.Is(err, io.EOF) {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, http.StatusRequestEntityTooLarge, err.Error()
		}
		return nil, http.StatusBadRequest, "invalid JSON: " + err.Error()
	}
	if params == nil {
		params = map[string]any{}
	}
	if id := r.PathValue("id"); id != "" {
		params["id"] = id
	}
	return params, 0, ""
}

func respond(c Command) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, status, errMsg := decode(r)
		var body any
		if status == 0 {
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
		if c != nil {
			method, path, _ := strings.Cut(pattern, " ")
			r.Routes[method+" "+r.Prefix+path] = c
		}
	}
	return r
}

func New(r Router) *Router {
	given := r.Routes
	r.Routes = map[string]Command{}
	return r.add(given).add(map[string]Command{"GET ": r.Select, "POST ": r.Create, "DELETE /{id}": r.Stop})
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
