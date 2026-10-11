package api

import (
	"context"
	"net/http"
	"time"
)

func (a *API) updateStatus(w http.ResponseWriter, r *http.Request) {
	write(w, http.StatusOK, a.Updates.Cached())
}

func (a *API) checkUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	write(w, http.StatusOK, a.Updates.Check(ctx))
}
