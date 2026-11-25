package web

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Metric alarms: a PromQL query of the organization's metrics compared with a
// threshold. The agent evaluates them every minute; a breach lasting the set minutes
// opens an incident and notifies the channels (event "metric_alarm").

type AlarmView struct {
	ID, Name, Query, Op, State, LastError string
	Threshold                             float64
	ForMinutes                            int
	Enabled                               bool
	LastValue                             *float64
	LastEval                              *time.Time
}

func (a AlarmView) ThresholdText() string { return strconv.FormatFloat(a.Threshold, 'g', -1, 64) }
func (a AlarmView) ValueText() string {
	if a.LastValue == nil {
		return "no data"
	}
	return strconv.FormatFloat(*a.LastValue, 'g', 4, 64)
}

func LoadAlarms(ctx context.Context, tx pgx.Tx) ([]AlarmView, error) {
	rows, err := tx.Query(ctx, `SELECT id, name, query, op, state, last_error, threshold, for_minutes, enabled, last_value, last_eval FROM alarm_rules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[AlarmView])
}
