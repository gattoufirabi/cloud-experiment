package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"platform/internal/db"
	"platform/internal/model"
	"platform/internal/secure"
)

// Message queues (like AWS SQS). Delivery is at least once: a received message is
// hidden for the visibility timeout and comes back unless deleted with its receipt
// handle; after max_receives receives it goes to the dead-letter queue. A function
// trigger consumes messages in batches (run by the agent).

const maxMessageBytes = 256 << 10

var queueNameRe = bucketNameRe // 3-40 lowercase letters, digits, dashes

type queueInfo struct {
	ID, Name, DLQ, Function                       string
	DLQID, FunctionID                             *string
	Visibility, Retention, MaxReceives, BatchSize int
	TriggerEnabled                                bool
	Available, InFlight, Delayed                  int64
	Created                                       time.Time
}

const queueCols = `q.id, q.name, coalesce(dq.name, ''), coalesce(p.slug || '/' || f.slug, ''), q.dlq_id, q.function_id,
	q.visibility_seconds, q.retention_seconds, q.max_receives, q.batch_size, q.trigger_enabled,
	(SELECT count(*) FROM queue_messages m WHERE m.queue_id = q.id AND m.visible_at <= now()),
	(SELECT count(*) FROM queue_messages m WHERE m.queue_id = q.id AND m.visible_at > now() AND m.receive_count > 0),
	(SELECT count(*) FROM queue_messages m WHERE m.queue_id = q.id AND m.visible_at > now() AND m.receive_count = 0),
	q.created_at`

const queueFrom = ` FROM queues q LEFT JOIN queues dq ON dq.id = q.dlq_id LEFT JOIN functions f ON f.id = q.function_id LEFT JOIN projects p ON p.id = f.project_id`

func scanQueue(row pgx.Row) (*queueInfo, error) {
	var q queueInfo
	err := row.Scan(&q.ID, &q.Name, &q.DLQ, &q.Function, &q.DLQID, &q.FunctionID, &q.Visibility, &q.Retention, &q.MaxReceives,
		&q.BatchSize, &q.TriggerEnabled, &q.Available, &q.InFlight, &q.Delayed, &q.Created)
	return &q, err
}

func loadQueues(ctx context.Context, tx pgx.Tx) ([]*queueInfo, error) {
	rows, err := tx.Query(ctx, `SELECT `+queueCols+queueFrom+` ORDER BY q.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*queueInfo
	for rows.Next() {
		q, err := scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func loadQueue(ctx context.Context, tx pgx.Tx, name string) (*queueInfo, error) {
	return scanQueue(tx.QueryRow(ctx, `SELECT `+queueCols+queueFrom+` WHERE q.name = $1`, name))
}

// RetentionText shows the retention in days or hours.
func (q *queueInfo) RetentionText() string {
	if q.Retention%86400 == 0 {
		return fmt.Sprintf("%d days", q.Retention/86400)
	}
	return fmt.Sprintf("%d hours", q.Retention/3600)
}

type queueMessage struct {
	ID            string            `json:"id"`
	Receipt       string            `json:"receipt,omitempty"`
	Body          string            `json:"body"`
	Attributes    map[string]string `json:"attributes,omitempty"`
	ReceiveCount  int               `json:"receive_count"`
	Sent          time.Time         `json:"sent_at"`
	FirstReceived *time.Time        `json:"first_received_at,omitempty"`
}

func meterQueue(ctx context.Context, tx pgx.Tx, orgID string, q *queueInfo, n int) {
	if n <= 0 {
		return
	}
	_, _ = tx.Exec(ctx, `
		INSERT INTO usage_hourly (org_id, resource_id, kind, name, hour, queue_requests) VALUES ($1, $2, 'queue', $3, date_trunc('hour', now()), $4)
		ON CONFLICT (resource_id, hour) DO UPDATE SET queue_requests = usage_hourly.queue_requests + EXCLUDED.queue_requests`,
		orgID, q.ID, q.Name, n)
}

func sendMessages(ctx context.Context, tx pgx.Tx, orgID string, q *queueInfo, msgs []queueMessage, delay int) ([]string, error) {
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if len(m.Body) == 0 || len(m.Body) > maxMessageBytes {
			return nil, apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("a message body is 1 byte to %d KB", maxMessageBytes>>10)}
		}
		attrs, _ := json.Marshal(m.Attributes)
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO queue_messages (org_id, queue_id, body, attributes, visible_at, expires_at)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5), now() + make_interval(secs => $6)) RETURNING id`,
			orgID, q.ID, m.Body, attrs, delay, q.Retention).Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	meterQueue(ctx, tx, orgID, q, 1)
	_, err := tx.Exec(ctx, `SELECT pg_notify('deployments', 'queue')`)
	return ids, err
}

// receiveMessages moves over-received messages to the dead-letter queue, then takes up
// to max visible messages and hides them for the visibility timeout.
func receiveMessages(ctx context.Context, tx pgx.Tx, q *queueInfo, max, visibility int) ([]queueMessage, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM queue_messages WHERE queue_id = $1 AND expires_at <= now()`, q.ID); err != nil {
		return nil, err
	}
	if q.DLQID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE queue_messages SET queue_id = $2, receive_count = 0, receipt = NULL, visible_at = now(), first_receive = NULL
			WHERE queue_id = $1 AND receive_count >= $3 AND visible_at <= now()`, q.ID, *q.DLQID, q.MaxReceives); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `
		WITH picked AS (
		    SELECT id FROM queue_messages WHERE queue_id = $1 AND visible_at <= now() AND expires_at > now()
		    ORDER BY sent_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE queue_messages m SET receive_count = m.receive_count + 1, receipt = $4 || replace(m.id::text, '-', '') || md5(random()::text),
		       visible_at = now() + make_interval(secs => $3), first_receive = coalesce(m.first_receive, now())
		FROM picked WHERE m.id = picked.id
		RETURNING m.id, m.receipt, m.body, m.attributes, m.receive_count, m.sent_at, m.first_receive`, q.ID, max, visibility, "r")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueMessage
	for rows.Next() {
		var m queueMessage
		var attrs []byte
		if err := rows.Scan(&m.ID, &m.Receipt, &m.Body, &attrs, &m.ReceiveCount, &m.Sent, &m.FirstReceived); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(attrs, &m.Attributes)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- REST API ----------

type apiQueue struct {
	Name        string    `json:"name"`
	Visibility  int       `json:"visibility_timeout_seconds"`
	Retention   int       `json:"retention_seconds"`
	MaxReceives int       `json:"max_receives"`
	DLQ         string    `json:"dead_letter_queue,omitempty"`
	Function    string    `json:"function,omitempty"`
	Available   int64     `json:"available"`
	InFlight    int64     `json:"in_flight"`
	Created     time.Time `json:"created_at"`
	URL         string    `json:"url"`
}

func (s *Server) apiQueueView(q *queueInfo) apiQueue {
	return apiQueue{Name: q.Name, Visibility: q.Visibility, Retention: q.Retention, MaxReceives: q.MaxReceives, DLQ: q.DLQ, Function: q.Function,
		Available: q.Available, InFlight: q.InFlight, Created: q.Created, URL: s.cfg.BaseURL + "/api/v1/queues/" + q.Name}
}

// withQueue loads {queue} in the token's organization; a queue-scoped token reaches
// only its own queue.
func (s *Server) withQueue(w http.ResponseWriter, r *http.Request, o *orgInfo, fn func(tx pgx.Tx, q *queueInfo) error) bool {
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		q, err := loadQueue(r.Context(), tx, r.PathValue("queue"))
		if err != nil {
			return err
		}
		if scope, _ := r.Context().Value(apiQueueScopeKey).(string); scope != "" && scope != q.ID {
			return pgx.ErrNoRows
		}
		return fn(tx, q)
	})
	return apiTx(w, s, err)
}

func (s *Server) apiQueues(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	out := []apiQueue{}
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		qs, err := loadQueues(r.Context(), tx)
		for _, q := range qs {
			out = append(out, s.apiQueueView(q))
		}
		return err
	})
	if apiTx(w, s, err) {
		writeJSON(w, out)
	}
}

type queueSettings struct {
	Name        string  `json:"name"`
	Visibility  *int    `json:"visibility_timeout_seconds"`
	Retention   *int    `json:"retention_seconds"`
	MaxReceives *int    `json:"max_receives"`
	DLQ         *string `json:"dead_letter_queue"`
}

// createQueueTx creates a queue with defaults or the given settings, within the plan.
func (s *Server) createQueueTx(r *http.Request, tx pgx.Tx, o *orgInfo, qs queueSettings) error {
	if !queueNameRe.MatchString(qs.Name) {
		return apiErr{http.StatusUnprocessableEntity, "queue name: 3 to 40 lowercase letters, digits and dashes"}
	}
	var n, max int
	if err := tx.QueryRow(r.Context(), `SELECT (SELECT count(*) FROM queues), max_queues FROM plans WHERE id = $1`, o.PlanID).Scan(&n, &max); err != nil {
		return err
	}
	if n >= max {
		return apiErr{http.StatusUnprocessableEntity, fmt.Sprintf("your plan allows %d queues", max)}
	}
	var id string
	if err := tx.QueryRow(r.Context(), `INSERT INTO queues (org_id, name) VALUES ($1, $2) RETURNING id`, o.ID, qs.Name).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return apiErr{http.StatusConflict, "a queue with this name already exists"}
		}
		return err
	}
	if err := s.updateQueueTx(r, tx, id, qs); err != nil {
		return err
	}
	s.audit(r, tx, o.ID, "queue.created", "queue:"+qs.Name, nil)
	return nil
}

func (s *Server) updateQueueTx(r *http.Request, tx pgx.Tx, id string, qs queueSettings) error {
	if qs.Visibility != nil && (*qs.Visibility < 1 || *qs.Visibility > 43200) {
		return apiErr{http.StatusUnprocessableEntity, "visibility timeout: 1 second to 12 hours"}
	}
	if qs.Retention != nil && (*qs.Retention < 60 || *qs.Retention > 1209600) {
		return apiErr{http.StatusUnprocessableEntity, "retention: 1 minute to 14 days"}
	}
	if qs.MaxReceives != nil && (*qs.MaxReceives < 1 || *qs.MaxReceives > 1000) {
		return apiErr{http.StatusUnprocessableEntity, "max receives: 1 to 1000"}
	}
	var dlq any
	setDLQ := qs.DLQ != nil
	if setDLQ && *qs.DLQ != "" {
		var did string
		if err := tx.QueryRow(r.Context(), `SELECT id FROM queues WHERE name = $1 AND id <> $2`, *qs.DLQ, id).Scan(&did); err != nil {
			return apiErr{http.StatusUnprocessableEntity, "the dead-letter queue must be another queue of the organization"}
		}
		dlq = did
	}
	_, err := tx.Exec(r.Context(), `
		UPDATE queues SET visibility_seconds = coalesce($2, visibility_seconds), retention_seconds = coalesce($3, retention_seconds),
		       max_receives = coalesce($4, max_receives), dlq_id = CASE WHEN $5 THEN $6::uuid ELSE dlq_id END
		WHERE id = $1`, id, qs.Visibility, qs.Retention, qs.MaxReceives, setDLQ, dlq)
	return err
}

func (s *Server) apiCreateQueue(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var qs queueSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&qs); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"name": "orders"}`)
		return
	}
	var view apiQueue
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		if err := s.createQueueTx(r, tx, o, qs); err != nil {
			return err
		}
		q, err := loadQueue(r.Context(), tx, qs.Name)
		if err == nil {
			view = s.apiQueueView(q)
		}
		return err
	})
	if apiTx(w, s, err) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(view)
	}
}

func (s *Server) apiDeleteQueue(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if s.withQueue(w, r, o, func(tx pgx.Tx, q *queueInfo) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM queues WHERE id = $1`, q.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "queue.deleted", "queue:"+q.Name, nil)
		return nil
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// apiSendMessages: {"body": "...", "attributes": {...}, "delay_seconds": 0} or
// {"messages": [{"body": "..."}, ...]} (up to 10).
func (s *Server) apiSendMessages(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	var body struct {
		Body       string            `json:"body"`
		Attributes map[string]string `json:"attributes"`
		Messages   []queueMessage    `json:"messages"`
		Delay      int               `json:"delay_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 11*maxMessageBytes)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, `body must be JSON, for example {"body": "hello"}`)
		return
	}
	msgs := body.Messages
	if body.Body != "" {
		msgs = append(msgs, queueMessage{Body: body.Body, Attributes: body.Attributes})
	}
	if len(msgs) == 0 || len(msgs) > 10 {
		apiError(w, http.StatusUnprocessableEntity, "send 1 to 10 messages")
		return
	}
	if body.Delay < 0 || body.Delay > 900 {
		apiError(w, http.StatusUnprocessableEntity, "delay_seconds: 0 to 900")
		return
	}
	var ids []string
	if s.withQueue(w, r, o, func(tx pgx.Tx, q *queueInfo) error {
		var err error
		ids, err = sendMessages(r.Context(), tx, o.ID, q, msgs, body.Delay)
		return err
	}) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"message_ids": ids})
	}
}

// apiReceive: ?max=1..10&wait=0..20 (long polling)&visibility=seconds.
func (s *Server) apiReceive(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	q0 := r.URL.Query()
	max, _ := strconv.Atoi(q0.Get("max"))
	if max < 1 || max > 10 {
		max = 1
	}
	wait, _ := strconv.Atoi(q0.Get("wait"))
	wait = min(max0(wait), 20)
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	var msgs []queueMessage
	for {
		ok := s.withQueue(w, r, o, func(tx pgx.Tx, q *queueInfo) error {
			vis := q.Visibility
			if v, err := strconv.Atoi(q0.Get("visibility")); err == nil && v >= 0 && v <= 43200 {
				vis = v
			}
			var err error
			msgs, err = receiveMessages(r.Context(), tx, q, max, vis)
			meterQueue(r.Context(), tx, o.ID, q, 1)
			return err
		})
		if !ok {
			return
		}
		if len(msgs) > 0 || time.Now().After(deadline) || r.Context().Err() != nil {
			break
		}
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	if msgs == nil {
		msgs = []queueMessage{}
	}
	writeJSON(w, map[string]any{"messages": msgs})
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func (s *Server) apiDeleteMessage(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	found := false
	if s.withQueue(w, r, o, func(tx pgx.Tx, q *queueInfo) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM queue_messages WHERE queue_id = $1 AND receipt = $2`, q.ID, r.PathValue("receipt"))
		found = err == nil && tag.RowsAffected() == 1
		meterQueue(r.Context(), tx, o.ID, q, 1)
		return err
	}) {
		if !found {
			apiError(w, http.StatusNotFound, "no message with this receipt handle (already deleted, or received again since)")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) apiPurgeQueue(w http.ResponseWriter, r *http.Request, o *orgInfo) {
	if scope, _ := r.Context().Value(apiQueueScopeKey).(string); scope != "" {
		apiError(w, http.StatusForbidden, "a queue token cannot purge")
		return
	}
	if s.withQueue(w, r, o, func(tx pgx.Tx, q *queueInfo) error {
		_, err := tx.Exec(r.Context(), `DELETE FROM queue_messages WHERE queue_id = $1`, q.ID)
		s.audit(r, tx, o.ID, "queue.purged", "queue:"+q.Name, nil)
		return err
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- console ----------

func (s *Server) queuesPage(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "viewer")
	if !ok {
		return
	}
	var qs []*queueInfo
	if err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		qs, err = loadQueues(r.Context(), tx)
		return err
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "queues", http.StatusOK, data{"Org": o, "Queues": qs, "CanEdit": model.RoleAtLeast(o.Role, "developer")})
}

func (s *Server) createQueue(w http.ResponseWriter, r *http.Request) {
	o, ok := s.org(w, r, "developer")
	if !ok {
		return
	}
	name := strings.TrimSpace(strings.ToLower(r.PostFormValue("name")))
	var ae apiErr
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error { return s.createQueueTx(r, tx, o, queueSettings{Name: name}) })
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/queues#create", "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/queues/"+name, "Queue "+name+" created.", "")
	}
}

func (s *Server) withQueuePage(w http.ResponseWriter, r *http.Request, minRole string, fn func(tx pgx.Tx, o *orgInfo, q *queueInfo) error) (*orgInfo, *queueInfo, error, bool) {
	o, ok := s.org(w, r, minRole)
	if !ok {
		return nil, nil, nil, false
	}
	var q *queueInfo
	err := db.WithOrg(r.Context(), s.pool, o.ID, func(tx pgx.Tx) error {
		var err error
		if q, err = loadQueue(r.Context(), tx, r.PathValue("queue")); err != nil {
			return err
		}
		return fn(tx, o, q)
	})
	if isNoRows(err) {
		s.notFound(w, r)
		return nil, nil, nil, false
	}
	return o, q, err, true
}

func (s *Server) queuePage(w http.ResponseWriter, r *http.Request) {
	var others []string
	var fns, services []string
	var peek []queueMessage
	o, q, err, ok := s.withQueuePage(w, r, "viewer", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		rows, err := tx.Query(r.Context(), `SELECT name FROM queues WHERE id <> $1 ORDER BY name`, q.ID)
		if err != nil {
			return err
		}
		if others, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT p.slug || '/' || f.slug FROM functions f JOIN projects p ON p.id = f.project_id ORDER BY 1`)
		if err != nil {
			return err
		}
		if fns, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `SELECT p.slug || '/' || s.slug FROM services s JOIN projects p ON p.id = s.project_id WHERE s.preview_of IS NULL ORDER BY 1`)
		if err != nil {
			return err
		}
		if services, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		rows, err = tx.Query(r.Context(), `
			SELECT id, '', body, attributes, receive_count, sent_at, first_receive FROM queue_messages
			WHERE queue_id = $1 ORDER BY sent_at LIMIT 20`, q.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m queueMessage
			var attrs []byte
			if err := rows.Scan(&m.ID, &m.Receipt, &m.Body, &attrs, &m.ReceiveCount, &m.Sent, &m.FirstReceived); err != nil {
				return err
			}
			if len(m.Body) > 300 {
				m.Body = m.Body[:300] + "…"
			}
			peek = append(peek, m)
		}
		return rows.Err()
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "queue", http.StatusOK, data{"Org": o, "Queue": q, "Others": others, "Functions": fns, "Services": services, "Peek": peek,
		"Path": o.Path() + "/queues/" + q.Name, "APIURL": s.cfg.BaseURL + "/api/v1/queues/" + q.Name, "Area": "queues",
		"CanEdit": model.RoleAtLeast(o.Role, "developer"), "CanDelete": model.RoleAtLeast(o.Role, "admin")})
}

func (s *Server) queueSendConsole(w http.ResponseWriter, r *http.Request) {
	body := r.PostFormValue("body")
	o, q, err, ok := s.withQueuePage(w, r, "developer", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		_, err := sendMessages(r.Context(), tx, o.ID, q, []queueMessage{{Body: body}}, 0)
		return err
	})
	if !ok {
		return
	}
	var ae apiErr
	switch {
	case errors.As(err, &ae):
		back(w, r, o.Path()+"/queues/"+q.Name, "", ae.msg)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/queues/"+q.Name, "Message sent.", "")
	}
}

func (s *Server) queueSettingsConsole(w http.ResponseWriter, r *http.Request) {
	vis, ret, maxr, batch := atoi(r.PostFormValue("visibility")), atoi(r.PostFormValue("retention_hours"))*3600, atoi(r.PostFormValue("max_receives")), atoi(r.PostFormValue("batch_size"))
	dlq := r.PostFormValue("dlq")
	fn := r.PostFormValue("function")
	trigger := r.PostFormValue("trigger_enabled") == "1"
	var formErr string
	o, q, err, ok := s.withQueuePage(w, r, "developer", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		if err := s.updateQueueTx(r, tx, q.ID, queueSettings{Visibility: &vis, Retention: &ret, MaxReceives: &maxr, DLQ: &dlq}); err != nil {
			var ae apiErr
			if errors.As(err, &ae) {
				formErr = ae.msg
				return nil
			}
			return err
		}
		var fnID any
		if fn != "" {
			ps, fs, _ := strings.Cut(fn, "/")
			var id string
			if err := tx.QueryRow(r.Context(), `SELECT f.id FROM functions f JOIN projects p ON p.id = f.project_id WHERE p.slug = $1 AND f.slug = $2`, ps, fs).Scan(&id); err != nil {
				formErr = "Choose one of your functions."
				return nil
			}
			fnID = id
		}
		if batch < 1 || batch > 10 {
			batch = 10
		}
		if _, err := tx.Exec(r.Context(), `UPDATE queues SET function_id = $2, batch_size = $3, trigger_enabled = $4 WHERE id = $1`, q.ID, fnID, batch, trigger); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "queue.updated", "queue:"+q.Name, map[string]any{"function": fn, "dlq": dlq, "visibility": vis})
		_, err := tx.Exec(r.Context(), `SELECT pg_notify('deployments', 'queue')`)
		return err
	})
	if !ok {
		return
	}
	switch {
	case formErr != "":
		back(w, r, o.Path()+"/queues/"+q.Name+"#settings", "", formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/queues/"+q.Name+"#settings", "Settings saved.", "")
	}
}

func (s *Server) queuePurgeConsole(w http.ResponseWriter, r *http.Request) {
	o, q, err, ok := s.withQueuePage(w, r, "developer", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		_, err := tx.Exec(r.Context(), `DELETE FROM queue_messages WHERE queue_id = $1`, q.ID)
		s.audit(r, tx, o.ID, "queue.purged", "queue:"+q.Name, nil)
		return err
	})
	if !ok {
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	back(w, r, o.Path()+"/queues/"+q.Name, "Queue purged.", "")
}

func (s *Server) queueDeleteConsole(w http.ResponseWriter, r *http.Request) {
	var formErr string
	o, q, err, ok := s.withQueuePage(w, r, "admin", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		if r.PostFormValue("confirm") != q.Name {
			formErr = "Type the queue name exactly to confirm deletion."
			return nil
		}
		if _, err := tx.Exec(r.Context(), `DELETE FROM queues WHERE id = $1`, q.ID); err != nil {
			return err
		}
		s.audit(r, tx, o.ID, "queue.deleted", "queue:"+q.Name, nil)
		return nil
	})
	if !ok {
		return
	}
	switch {
	case formErr != "":
		back(w, r, o.Path()+"/queues/"+q.Name+"#settings", "", formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/queues", "Queue "+q.Name+" deleted.", "")
	}
}

// queueAttach gives a service a token for this queue only, stored as its secrets
// QUEUE_URL and QUEUE_TOKEN.
func (s *Server) queueAttach(w http.ResponseWriter, r *http.Request) {
	target := r.PostFormValue("service")
	var formErr string
	o, q, err, ok := s.withQueuePage(w, r, "developer", func(tx pgx.Tx, o *orgInfo, q *queueInfo) error {
		ps, ss, _ := strings.Cut(target, "/")
		var serviceID string
		if err := tx.QueryRow(r.Context(), `SELECT s.id FROM services s JOIN projects p ON p.id = s.project_id WHERE p.slug = $1 AND s.slug = $2 AND s.preview_of IS NULL`,
			ps, ss).Scan(&serviceID); err != nil {
			formErr = "Choose a service."
			return nil
		}
		token := apiTokenPrefix + secure.NewToken()
		if _, err := tx.Exec(r.Context(), `
			INSERT INTO api_tokens (org_id, name, token_hash, role, created_by, queue_id) VALUES ($1, $2, $3, 'developer', $4, $5)`,
			o.ID, truncateStr("queue "+q.Name+" for "+target, 60), secure.HashToken(token), nullableUser(r), q.ID); err != nil {
			return err
		}
		vars := map[string]string{"QUEUE_URL": s.cfg.BaseURL + "/api/v1/queues/" + q.Name, "QUEUE_TOKEN": token}
		for name, value := range vars {
			enc, err := s.box.Seal([]byte(value), "secret:"+serviceID+":"+name)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(r.Context(), `
				INSERT INTO secrets (org_id, service_id, name, value_enc, source) VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (service_id, name) DO UPDATE SET value_enc = EXCLUDED.value_enc, source = EXCLUDED.source, created_at = now()`, o.ID, serviceID, name, enc, "queue:"+q.ID); err != nil {
				return err
			}
		}
		s.audit(r, tx, o.ID, "queue.attached", "queue:"+q.Name, map[string]string{"service": target})
		return nil
	})
	if !ok {
		return
	}
	switch {
	case formErr != "":
		back(w, r, o.Path()+"/queues/"+q.Name+"#connect", "", formErr)
	case err != nil:
		s.serverError(w, r, err)
	default:
		back(w, r, o.Path()+"/queues/"+q.Name+"#connect", "Secrets QUEUE_URL and QUEUE_TOKEN added to "+target+". Redeploy it to apply them.", "")
	}
}
