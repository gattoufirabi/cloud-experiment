package web

import (
	"errors"
	"net/http"
)

func APITx(w http.ResponseWriter, s *Server, err error) bool {
	var ae APIErr
	switch {
	case err == nil:
		return true
	case errors.As(err, &ae):
		APIError(w, ae.Status, ae.Msg)
	case IsNoRows(err):
		APIError(w, http.StatusNotFound, "not found")
	default:
		s.Log.Error("functions api", "err", err)
		APIError(w, http.StatusInternalServerError, "internal error")
	}
	return false
}
