package main

import (
	"net/http"

	"github.com/antresol-app/antresol/addonkit"
)

// newHandler serves /health and the hooks; the files.* hooks are
// registered on the returned HookMux once the services are built (until
// then they answer 404: the core fails the operation closed).
func newHandler(env addonkit.Env, core *addonkit.Client, ready *addonkit.Ready) (http.Handler, *addonkit.HookMux) {
	mux := http.NewServeMux()
	// the core's health check: {"ok": true, "ready": …}
	mux.HandleFunc("GET /health", ready.Health)
	// the hooks the core calls (POST /hooks/<name>)
	hooks := &addonkit.HookMux{}
	mux.Handle("POST /hooks/", hooks)
	return mux, hooks
}
