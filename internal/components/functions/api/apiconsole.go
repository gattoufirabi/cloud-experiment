package api

import (
	"platform/internal/core/web"
)

func extractWebhook(page string) map[string]string {
	if u := web.WebhookURLRe.FindString(page); u != "" {
		return map[string]string{"url": u}
	}
	return nil
}
