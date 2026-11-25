package api

import (
	"net/http"

	"platform/internal/core/web"
)

// Routes registers this component's pages and API endpoints on the public listener.
func (s *API) Routes(m *http.ServeMux) {
	s.apiConsoleRoutes(m)
	s.apiRoutes(m)
	s.consoleRoutes(m)
}

// apiConsoleRoutes: this component's part of the former apiConsoleRoutes table.
func (s *API) apiConsoleRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/storage/buckets/{bucket}/attach", s.APIAuth("viewer", s.APIConsole(s.attachBucket, nil)))
}

// apiRoutes: this component's part of the former apiRoutes table.
func (s *API) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/buckets", s.APIAuth("viewer", s.apiBuckets))
	m.HandleFunc("POST /api/v1/buckets", s.APIAuth("developer", s.apiCreateBucket))
	m.HandleFunc("PUT /api/v1/buckets/{bucket}/cors", s.APIAuth("developer", s.apiBucketCORS))
	m.HandleFunc("DELETE /api/v1/buckets/{bucket}", s.APIAuth("admin", s.apiDeleteBucket))
	m.HandleFunc("GET /api/v1/storage-keys", s.APIAuth("viewer", s.apiStorageKeys))
	m.HandleFunc("POST /api/v1/storage-keys", s.APIAuth("developer", s.apiCreateStorageKey))
	m.HandleFunc("DELETE /api/v1/storage-keys/{id}", s.APIAuth("developer", s.apiDeleteStorageKey))
}

// consoleRoutes: this component's part of the former routes table.
func (s *API) consoleRoutes(m *http.ServeMux) {
	const st = "/o/{org}/storage"
	m.HandleFunc("GET "+st, s.Auth(web.AuthFull, s.storagePage))
	m.HandleFunc("POST "+st+"/buckets", s.Auth(web.AuthFull, s.createBucket))
	m.HandleFunc("POST "+st+"/keys", s.Auth(web.AuthFull, s.createStorageKey))
	m.HandleFunc("POST "+st+"/keys/{id}/delete", s.Auth(web.AuthFull, s.deleteStorageKey))
	m.HandleFunc("GET "+st+"/b/{bucket}", s.Auth(web.AuthFull, s.bucketPage))
	m.HandleFunc("GET "+st+"/b/{bucket}/settings", s.Auth(web.AuthFull, web.SettingsView(s.bucketPage)))
	m.HandleFunc("POST "+st+"/b/{bucket}/cors", s.Auth(web.AuthFull, s.bucketCORS))
	m.HandleFunc("POST "+st+"/b/{bucket}/settings", s.Auth(web.AuthFull, s.bucketSettings))
	m.HandleFunc("POST "+st+"/b/{bucket}/attach", s.Auth(web.AuthFull, s.attachBucket))
	m.HandleFunc("POST "+st+"/b/{bucket}/delete", s.Auth(web.AuthFull, s.deleteBucket))
	m.HandleFunc("POST "+st+"/b/{bucket}/objects/upload", s.Auth(web.AuthFull, s.uploadObject))
	m.HandleFunc("POST "+st+"/b/{bucket}/objects/delete", s.Auth(web.AuthFull, s.deleteObject))
	m.HandleFunc("GET "+st+"/b/{bucket}/objects/download", s.Auth(web.AuthFull, s.downloadObject))
}
