package collie

import (
	"context"
	"log"
	"net/http"
	"strings"
)

func (s *Server) inboxSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	matches, err := s.ext.store.searchMessages(r.Context(), query)
	if err != nil {
		log.Printf("inbox: search messages: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "未能搜索收件箱消息"})
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"matches": matches})
}

func (st *extStore) searchMessages(ctx context.Context, query string) ([]string, error) {
	matches := []string{}
	if query == "" {
		return matches, nil
	}
	// Match the history the inbox can show from storage. Closed chats only
	// expose Collie's sends from this table, as in inboxConv.
	rows, err := st.db.QueryContext(ctx, `SELECT m.conv, json_extract(m.data, '$.text')
		FROM ext_msgs m JOIN ext_convs c ON c.id = m.conv
		WHERE json_extract(c.data, '$.on') = 1 OR json_extract(m.data, '$.ref') <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}
		if !seen[id] && strings.Contains(strings.ToLower(text), query) {
			seen[id] = true
			matches = append(matches, id)
		}
	}
	return matches, rows.Err()
}
