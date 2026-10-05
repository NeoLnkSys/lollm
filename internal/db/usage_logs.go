package db

import (
	"context"
	"database/sql"
	"fmt"
)

const usageLogCols = `id, request_id, api_key_id, combo_name, connection_id, model, agent_role,
	prompt_tokens, completion_tokens, tokens_saved, latency_ms, status_code, error, created_at`

func scanUsageLog(r rowScanner) (*UsageLog, error) {
	var (
		l                              UsageLog
		apiKeyID, combo, connID, model sql.NullString
		role, errText, created         sql.NullString
	)
	if err := r.Scan(&l.ID, &l.RequestID, &apiKeyID, &combo, &connID, &model, &role,
		&l.PromptTokens, &l.CompletionTokens, &l.TokensSaved, &l.LatencyMs,
		&l.StatusCode, &errText, &created); err != nil {
		return nil, err
	}
	l.APIKeyID = apiKeyID.String
	l.ComboName = combo.String
	l.ConnectionID = connID.String
	l.Model = model.String
	l.AgentRole = role.String
	l.Error = errText.String
	l.CreatedAt = parseTime(created)
	return &l, nil
}

// InsertUsageLog records one proxied call (or one internal agent step).
func (s *Store) InsertUsageLog(ctx context.Context, l *UsageLog) error {
	if l.ID == "" {
		l.ID = NewID("log_")
	}
	l.CreatedAt, _ = nowPair()
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_logs
		(id, request_id, api_key_id, combo_name, connection_id, model, agent_role,
		 prompt_tokens, completion_tokens, tokens_saved, latency_ms, status_code, error, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		l.ID, l.RequestID, nullStr(l.APIKeyID), nullStr(l.ComboName), nullStr(l.ConnectionID),
		nullStr(l.Model), nullStr(l.AgentRole), l.PromptTokens, l.CompletionTokens,
		l.TokensSaved, l.LatencyMs, l.StatusCode, nullStr(l.Error), nowUTC())
	if err != nil {
		return fmt.Errorf("db: insert usage log: %w", err)
	}
	return nil
}

// ListUsageLogs returns the most recent logs (limit capped at 500).
func (s *Store) ListUsageLogs(ctx context.Context, limit int) ([]*UsageLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+usageLogCols+` FROM usage_logs ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UsageLog
	for rows.Next() {
		l, err := scanUsageLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
