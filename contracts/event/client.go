package event

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to the plect event bus API over HTTP/SSE. The transport is
// set by BaseURL + HTTP: NewUDSClient dials a Unix domain socket; tests
// inject an httptest base URL. The bus speaks session_name only; provider-
// specific details ride opaquely in Event.Metadata.
type Client struct {
	BaseURL string // e.g. "http://unix" (UDS) or an httptest URL
	Token   string // bearer; empty for same-user UDS
	HTTP    *http.Client
}

// NewUDSClient builds a Client whose HTTP transport dials the given Unix socket.
func NewUDSClient(socketPath, token string) *Client {
	return &Client{
		BaseURL: "http://unix",
		Token:   token,
		HTTP: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

func (c *Client) auth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// Publish appends an event and returns its assigned id and sequence.
func (c *Client) Publish(ctx context.Context, ev Event) (id string, off int64, err error) {
	body, err := json.Marshal(ev)
	if err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/events", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("publish: %s", resp.Status)
	}
	var out struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	return out.ID, out.Offset, nil
}

// listResponse is the wire shape of GET /v1/events. The cursor is opaque:
// clients pass NextCursor back verbatim and never interpret it.
type listResponse struct {
	Events     []Event `json:"events"`
	NextCursor string  `json:"next_cursor"`
}

// List returns one page of a session's events in the given order, filtered by
// f, plus the opaque token for the next page (empty when there is none). An
// empty cursor starts from the head (asc) or the most recent page (desc).
// session rides as a query param (not a path segment) to avoid the %2F-in-path
// footgun for names like "session-1".
func (c *Client) List(ctx context.Context, session string, order Order, cursor string, f Filter) (evs []Event, nextCursor string, err error) {
	u := c.BaseURL + "/v1/events?" + listQuery(session, order, cursor, f).Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	c.auth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("list: %s", resp.Status)
	}
	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	return out.Events, out.NextCursor, nil
}

// Subscribe streams events for a session from sequence `since`, calling fn
// for each. It replays from the log then follows live (one SSE path), and
// reconnects with Last-Event-ID so reconnects don't drop events. The caller is
// expected to dedup by Event.ID across reconnects. Returns when ctx is done.
func (c *Client) Subscribe(ctx context.Context, session string, since int64, f Filter, fn func(Event, int64)) error {
	backoff := 200 * time.Millisecond
	var streamID string
	cursor := since
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		nextStreamID, nextCursor, _, err := c.streamOnce(ctx, session, streamID, cursor, f, fn)
		streamID, cursor = nextStreamID, nextCursor
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			backoff = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// streamOnce opens a single SSE connection and dispatches events until the stream ends or ctx is cancelled, returning the (streamID, sequence) to resume from on the next reconnect and the number of events delivered.
func (c *Client) streamOnce(ctx context.Context, session, streamID string, since int64, f Filter, fn func(Event, int64)) (nextStreamID string, nextSince int64, count int, err error) {
	var resumeToken string
	if since > 0 || streamID != "" {
		resumeToken = EncodeResumeToken(streamID, since)
	}
	u := c.BaseURL + "/v1/stream?" + filterQuery(session, resumeToken, f).Encode()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if rerr != nil {
		return streamID, since, 0, rerr
	}
	req.Header.Set("Accept", "text/event-stream")
	if resumeToken != "" {
		req.Header.Set("Last-Event-ID", resumeToken)
	}
	c.auth(req)
	resp, derr := c.HTTP.Do(req)
	if derr != nil {
		return streamID, since, 0, derr
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return streamID, since, 0, fmt.Errorf("subscribe: %s", resp.Status)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var dataLines []string
	lastStreamID, lastSeq := streamID, since
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "": // dispatch on blank line
			if len(dataLines) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &ev); err == nil {
				fn(ev, lastSeq)
				count++
			}
			dataLines = dataLines[:0]
		case strings.HasPrefix(line, "id:"):
			if sid, seq, ok := ParseResumeToken(strings.TrimSpace(line[3:])); ok {
				lastStreamID, lastSeq = sid, seq
			}
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line[5:], " "), ""))
		}
	}
	return lastStreamID, lastSeq, count, sc.Err()
}

// addFilterParams encodes the type/source/direction/delivery_mode/limit
// selection shared by the list and stream query shapes.
func addFilterParams(q url.Values, f Filter) {
	if len(f.Types) > 0 {
		q.Set("types", strings.Join(f.Types, ","))
	}
	if len(f.Sources) > 0 {
		q.Set("source", strings.Join(f.Sources, ","))
	}
	if f.Direction != "" {
		q.Set("direction", string(f.Direction))
	}
	if f.DeliveryMode != "" {
		q.Set("delivery_mode", string(f.DeliveryMode))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
}

// listQuery encodes session + order + opaque cursor + filter for GET /v1/events.
// session rides as a query param (not a path segment) to avoid the %2F-in-path
// footgun for names like "session-1".
func listQuery(session string, order Order, cursor string, f Filter) url.Values {
	q := url.Values{}
	q.Set("session", session)
	if order != "" && order != OrderAsc {
		q.Set("order", string(order))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	addFilterParams(q, f)
	return q
}

// filterQuery encodes session + resume token + filter for the streaming path (GET /v1/stream); resumeToken is "" for a fresh connect.
func filterQuery(session, resumeToken string, f Filter) url.Values {
	q := url.Values{}
	q.Set("session", session)
	if resumeToken != "" {
		q.Set("since", resumeToken)
	}
	addFilterParams(q, f)
	return q
}
