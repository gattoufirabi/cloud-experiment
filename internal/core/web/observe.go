package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type LogLine struct {
	Time   string `json:"time"`
	Msg    string `json:"msg"`
	Stream string `json:"stream"`
}

// queryLogs returns up to 300 lines of the last 6 hours for one service, oldest first.
// The tenant and service filters are fixed here; the search text is always quoted.
func (s *Server) QueryLogs(orgID, serviceID, search string) []LogLine {
	out := []LogLine{}
	if s.Cfg.VictoriaLogsURL == "" {
		return out
	}
	search = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, search)
	if len(search) > 100 {
		search = search[:100]
	}
	q := fmt.Sprintf(`_time:6h org_id:=%s service_id:=%s`, strconv.Quote(orgID), strconv.Quote(serviceID))
	if search != "" {
		q += " " + strconv.Quote(search)
	}
	q += " | sort by (_time desc) | limit 300"
	resp, err := s.HTTP.Get(s.Cfg.VictoriaLogsURL + "/select/logsql/query?" + url.Values{"query": {q}}.Encode())
	if err != nil {
		s.Log.Warn("query logs", "err", err)
		return out
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, LogLine{Time: m["_time"], Msg: m["_msg"], Stream: m["stream"]})
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // oldest first
		out[i], out[j] = out[j], out[i]
	}
	return out
}
