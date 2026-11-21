package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DBLog is one persisted backend log row from the server_logs table.
type DBLog struct {
	ID        int64
	CreatedAt time.Time
	Level     string
	Tag       string
	Text      string
}

// InsertLog appends one log line and returns its auto-assigned id.
func (d *DB) InsertLog(level, tag, text string) (int64, error) {
	var id int64
	err := d.QueryRowContext(context.Background(),
		"INSERT INTO server_logs(level,tag,text) VALUES($1,$2,$3) RETURNING id",
		level, tag, text,
	).Scan(&id)
	return id, err
}

// InsertLogsBatch persists a batch of log entries in a single multi-row INSERT
// statement and returns their auto-assigned ids in input order.
func (d *DB) InsertLogsBatch(entries []DBLog) ([]int64, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) == 1 {
		id, err := d.InsertLog(entries[0].Level, entries[0].Tag, entries[0].Text)
		if err != nil {
			return nil, err
		}
		return []int64{id}, nil
	}
	var sb strings.Builder
	sb.WriteString("INSERT INTO server_logs(level,tag,text) VALUES ")
	args := make([]any, 0, len(entries)*3)
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*3 + 1
		fmt.Fprintf(&sb, "($%d,$%d,$%d)", base, base+1, base+2)
		args = append(args, e.Level, e.Tag, e.Text)
	}
	sb.WriteString(" RETURNING id")
	rows, err := d.QueryContext(context.Background(), sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, len(entries))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RecentLogs returns the most recent limit rows, oldest-first.
func (d *DB) RecentLogs(limit int) ([]*DBLog, error) {
	rows, err := d.QueryContext(context.Background(),
		`SELECT id, created_at, level, tag, text
		   FROM server_logs
		  ORDER BY id DESC
		  LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DBLog
	for rows.Next() {
		l := &DBLog{}
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.Level, &l.Tag, &l.Text); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	// Reverse to oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// ListLogsBefore returns up to limit rows with id < beforeID, oldest-first.
func (d *DB) ListLogsBefore(beforeID int64, limit int) ([]*DBLog, error) {
	rows, err := d.QueryContext(context.Background(),
		`SELECT id, created_at, level, tag, text
		   FROM server_logs
		  WHERE id < $1
		  ORDER BY id DESC
		  LIMIT $2`, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DBLog
	for rows.Next() {
		l := &DBLog{}
		if err := rows.Scan(&l.ID, &l.CreatedAt, &l.Level, &l.Tag, &l.Text); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	// Reverse to oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
