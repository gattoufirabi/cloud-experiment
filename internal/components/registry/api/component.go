package api

import (
	"platform/internal/core/web"
)

// API is this component's part of the console and API: it embeds the core (internal/core/web)
// and adds this feature's pages and endpoints.
type API struct{ *web.Server }
