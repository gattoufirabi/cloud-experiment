package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"platform/internal/lib/model"
)

// resolveSecretRefs replaces ${secret:NAME} values with the current version of the
// organization's secret. A missing secret fails the deployment with a clear message.
func (a *Agent) ResolveSecretRefs(ctx context.Context, orgID string, env []string) ([]string, error) {
	out := make([]string, len(env))
	cache := map[string]string{}
	for i, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		name, ok := model.SecretRef(v)
		if !ok {
			out[i] = kv
			continue
		}
		val, seen := cache[name]
		if !seen {
			var id string
			var version int
			var enc []byte
			err := a.Pool.QueryRow(ctx, `
				SELECT s.id, v.version, v.value_enc FROM org_secrets s JOIN org_secret_versions v ON v.secret_id = s.id AND v.version = s.current_version
				WHERE s.org_id = $1 AND s.name = $2`, orgID, name).Scan(&id, &version, &enc)
			if err != nil {
				return nil, fmt.Errorf("variable %s references secret %q, which does not exist in the secrets manager", k, name)
			}
			plain, err := a.Box.Open(enc, "orgsecret:"+id+":"+strconv.Itoa(version))
			if err != nil {
				return nil, fmt.Errorf("decrypt secret %q: %w", name, err)
			}
			val = string(plain)
			cache[name] = val
		}
		out[i] = k + "=" + val
	}
	return out, nil
}
