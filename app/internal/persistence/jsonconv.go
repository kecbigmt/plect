package persistence

import (
	"database/sql"
	"encoding/json"
)

// marshalJSONMap encodes m for a nullable _json column: a nil/empty map is
// SQL NULL, matching formatTimeNull/nullString's absence convention.
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

// marshalJSONValue treats a nil pointer/interface as genuinely absent.
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

// unmarshalJSONValue leaves *out untouched (its zero value) when s is NULL.
func unmarshalJSONValue(s sql.NullString, out any) error {
	if !s.Valid {
		return nil
	}
	return json.Unmarshal([]byte(s.String), out)
}

// nullInt always reports Valid: a layer's heartbeat counters are a
// legitimate 0, not an absent fact worth a NULL.
func nullInt(v int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(v), Valid: true}
}
