package persistence

import (
	"database/sql"
	"encoding/json"
)

// marshalJSONMap encodes m for a nullable _json column: a nil (or empty)
// map is genuinely absent (SQL NULL), matching the same real-absence-is-
// NULL convention formatTimeNull/nullString use.
func marshalJSONMap(m map[string]any) (sql.NullString, error) {
	if len(m) == 0 {
		return sql.NullString{}, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(data), Valid: true}, nil
}

// unmarshalJSONMap is marshalJSONMap's inverse.
func unmarshalJSONMap(s sql.NullString) (map[string]any, error) {
	if !s.Valid {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s.String), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// marshalJSONValue encodes any JSON-marshalable value for a nullable _json
// column, treating a nil pointer/interface as genuinely absent.
func marshalJSONValue(v any) (sql.NullString, error) {
	if v == nil {
		return sql.NullString{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(data), Valid: true}, nil
}

// unmarshalJSONValue decodes a nullable _json column into *out, leaving
// *out untouched (its zero value) when the column is NULL.
func unmarshalJSONValue(s sql.NullString, out any) error {
	if !s.Valid {
		return nil
	}
	return json.Unmarshal([]byte(s.String), out)
}

// nullInt always reports Valid: a layer's own heartbeat counters are a
// legitimate 0 rather than an absent fact, so there is no "never set"
// distinction worth a NULL here (see node_instance_layers/
// task_instance_layers' heartbeat_ticks/heartbeat_escalations columns).
func nullInt(v int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(v), Valid: true}
}
