package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.consoleRoutes(m)
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /o/{org}/servers", s.Auth(web.AuthFull, s.serversPage))
	m.HandleFunc("POST /o/{org}/servers", s.Auth(web.AuthFull, s.createServerJoin))
	m.HandleFunc("POST /o/{org}/servers/place", s.Auth(web.AuthFull, s.placeProject))
	m.HandleFunc("POST /o/{org}/servers/{id}/delete", s.Auth(web.AuthFull, s.deleteServer))
	m.HandleFunc("POST /o/{org}/servers/{id}/builds", s.Auth(web.AuthFull, s.serverBuildSettings))
	m.HandleFunc("GET /node/install.sh", s.nodeInstallScript)
	m.HandleFunc("GET /node/v1/agent/{platform}", s.nodeAgentBinary)
	m.HandleFunc("POST /node/v1/join", s.nodeJoin)
	m.HandleFunc("POST /node/v1/heartbeat", s.nodeAuth(s.nodeHeartbeat))
	m.HandleFunc("GET /node/v1/desired", s.nodeAuth(s.nodeDesired))
	m.HandleFunc("POST /node/v1/deployments/{id}/status", s.nodeAuth(s.nodeDeploymentStatus))
	m.HandleFunc("POST /node/v1/logs", s.nodeAuth(s.nodeLogs))
	m.HandleFunc("POST /node/v1/stats", s.nodeAuth(s.nodeStats))
	m.HandleFunc("GET /node/v1/tunnel", s.nodeAuth(s.nodeTunnel))
	m.HandleFunc("POST /node/v1/builds/claim", s.nodeAuth(s.nodeBuildClaim))
	m.HandleFunc("POST /node/v1/builds/{id}/log", s.nodeAuth(s.nodeBuildLog))
	m.HandleFunc("GET /node/v1/builds/{id}/source", s.nodeAuth(s.nodeBuildSource))
	m.HandleFunc("POST /node/v1/builds/{id}/secrets", s.nodeAuth(s.nodeBuildSecrets))
	m.HandleFunc("POST /node/v1/builds/{id}/finish", s.nodeAuth(s.nodeBuildFinish))
}
