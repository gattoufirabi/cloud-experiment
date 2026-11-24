package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/core/worker"
)

func (a *Worker) QueueLoop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var lastPrune time.Time
	for {
		a.settleDeliveries(ctx)
		a.feedQueueTriggers(ctx)
		if a.IsPrimary() && time.Since(lastPrune) > 10*time.Minute {
			lastPrune = time.Now()
			_, _ = a.Pool.Exec(ctx, `DELETE FROM queue_messages WHERE expires_at <= now()`)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.QueueWake:
		}
	}
}

type queueTrigger struct {
	ID, OrgID, Name, OrgSlug, FunctionID string
	DLQID                                *string
	Visibility, MaxReceives, BatchSize   int
	Concurrency                          int
}

func (a *Worker) feedQueueTriggers(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT q.id, q.org_id, q.name, o.slug, f.id, q.dlq_id, q.visibility_seconds, q.max_receives, q.batch_size, f.concurrency
		FROM queues q JOIN functions f ON f.id = q.function_id JOIN projects p ON p.id = f.project_id JOIN orgs o ON o.id = q.org_id
		WHERE q.trigger_enabled AND NOT o.suspended AND `+worker.MineSQL, a.MineArgs()...)
	if err != nil {
		return
	}
	triggers, err := pgx.CollectRows(rows, pgx.RowToStructByPos[queueTrigger])
	if err != nil {
		return
	}
	fed := false
	for _, t := range triggers {
		var inflight int
		if err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM queue_deliveries WHERE queue_id = $1`, t.ID).Scan(&inflight); err != nil {
			continue
		}
		for ; inflight < t.Concurrency; inflight++ {
			n, err := a.deliverBatch(ctx, t)
			if err != nil {
				a.Log.Warn("queue trigger", "queue", t.Name, "err", err)
				break
			}
			if n == 0 {
				break
			}
			fed = true
		}
	}
	if fed {
		a.PokeFn()
	}
}

// deliverBatch receives up to batch_size messages and queues one function invocation.
func (a *Worker) deliverBatch(ctx context.Context, t queueTrigger) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
		if t.DLQID != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE queue_messages SET queue_id = $2, receive_count = 0, receipt = NULL, visible_at = now(), first_receive = NULL
				WHERE queue_id = $1 AND receive_count >= $3 AND visible_at <= now()`, t.ID, *t.DLQID, t.MaxReceives); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, `
			WITH picked AS (
			    SELECT id FROM queue_messages WHERE queue_id = $1 AND visible_at <= now() AND expires_at > now()
			      AND receive_count < CASE WHEN $4::boolean THEN $5 ELSE 2147483647 END
			    ORDER BY sent_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			UPDATE queue_messages m SET receive_count = m.receive_count + 1, receipt = 'f' || replace(m.id::text, '-', '') || md5(random()::text),
			       visible_at = now() + make_interval(secs => $3), first_receive = coalesce(m.first_receive, now())
			FROM picked WHERE m.id = picked.id
			RETURNING m.id, m.receipt, m.body, m.attributes, m.receive_count, m.sent_at, m.first_receive`,
			t.ID, t.BatchSize, t.Visibility, t.DLQID != nil, t.MaxReceives)
		if err != nil {
			return err
		}
		var ids []string
		var records []map[string]any
		for rows.Next() {
			var id, receipt, body string
			var attrs []byte
			var count int
			var sent time.Time
			var first *time.Time
			if err := rows.Scan(&id, &receipt, &body, &attrs, &count, &sent, &first); err != nil {
				rows.Close()
				return err
			}
			var msgAttrs map[string]string
			_ = json.Unmarshal(attrs, &msgAttrs)
			ma := map[string]any{}
			for k, v := range msgAttrs {
				ma[k] = map[string]string{"stringValue": v, "dataType": "String"}
			}
			firstMS := sent.UnixMilli()
			if first != nil {
				firstMS = first.UnixMilli()
			}
			ids = append(ids, id)
			records = append(records, map[string]any{
				"messageId": id, "receiptHandle": receipt, "body": body, "md5OfBody": "",
				"attributes": map[string]string{"ApproximateReceiveCount": strconv.Itoa(count), "SentTimestamp": strconv.FormatInt(sent.UnixMilli(), 10),
					"ApproximateFirstReceiveTimestamp": strconv.FormatInt(firstMS, 10)},
				"messageAttributes": ma, "eventSource": "platform:queue", "eventSourceARN": "plat:queue:" + t.OrgSlug + ":" + t.Name, "queue": t.Name,
			})
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		event, _ := json.Marshal(map[string]any{"Records": records})
		var invID string
		if err := tx.QueryRow(ctx, `INSERT INTO function_invocations (org_id, function_id, trigger, async, event) VALUES ($1, $2, 'queue', true, $3) RETURNING id`,
			t.OrgID, t.FunctionID, event).Scan(&invID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO queue_deliveries (invocation_id, org_id, queue_id, message_ids) VALUES ($1, $2, $3, $4)`,
			invID, t.OrgID, t.ID, ids); err != nil {
			return err
		}
		n = len(ids)
		return nil
	})
	return n, err
}

// settleDeliveries deletes the messages of finished, successful invocations.
func (a *Worker) settleDeliveries(ctx context.Context) {
	rows, err := a.Pool.Query(ctx, `
		SELECT d.invocation_id, d.message_ids, i.status, coalesce(i.response, '\x'::bytea)
		FROM queue_deliveries d JOIN function_invocations i ON i.id = d.invocation_id
		WHERE i.status IN ('succeeded', 'failed', 'timed_out')`)
	if err != nil {
		return
	}
	type done struct {
		inv, status string
		ids         []string
		resp        []byte
	}
	var list []done
	for rows.Next() {
		var d done
		if rows.Scan(&d.inv, &d.ids, &d.status, &d.resp) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		_ = pgx.BeginFunc(ctx, a.Pool, func(tx pgx.Tx) error {
			if d.status == "succeeded" {
				failed := batchItemFailures(d.resp)
				var del []string
				for _, id := range d.ids {
					if !failed[id] {
						del = append(del, id)
					}
				}
				if _, err := tx.Exec(ctx, `DELETE FROM queue_messages WHERE id = ANY($1)`, del); err != nil {
					return err
				}
			} // failed or timed out: the messages come back after their visibility timeout
			if _, err := tx.Exec(ctx, `DELETE FROM queue_deliveries WHERE invocation_id = $1`, d.inv); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE function_invocations SET response = NULL WHERE id = $1`, d.inv)
			return err
		})
	}
	if len(list) > 0 {
		a.PokeQueues()
	}
}

// batchItemFailures reads {"batchItemFailures": [{"itemIdentifier": id}]} from a result.
func batchItemFailures(resp []byte) map[string]bool {
	var r struct {
		Result struct {
			Failures []struct {
				ID string `json:"itemIdentifier"`
			} `json:"batchItemFailures"`
		} `json:"result"`
	}
	out := map[string]bool{}
	if json.Unmarshal(resp, &r) == nil {
		for _, f := range r.Result.Failures {
			out[f.ID] = true
		}
	}
	return out
}
