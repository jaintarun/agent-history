package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// SearchQuery contains composable session retrieval filters.
type SearchQuery struct {
	Text            string
	IncludeMessages bool
	Agent           string
	ActiveAfter     *time.Time
	ActiveBefore    *time.Time
	StartedAfter    *time.Time
	StartedBefore   *time.Time
	CWD             string
	TopicMode       string
	AnalysisStatus  string
	Cmux            string
	Sort            string
	Cursor          string
	Limit           int
}

// SessionHit is one filtered session plus its best text-match snippet.
type SessionHit struct {
	Session Session
	Snippet string
	Score   float64
}

// SearchResult is one stable cursor page.
type SearchResult struct {
	Hits       []SessionHit
	NextCursor string
}

// FacetCount is one value/count pair.
type FacetCount struct {
	Value string
	Count int
}

// Facets summarizes the available local history.
type Facets struct {
	Agents         map[string]int
	Directories    []FacetCount
	AnalysisStates map[string]int
	StartedMin     time.Time
	LastActiveMax  time.Time
}

type textMatch struct {
	snippet string
	score   float64
}

type searchCursor struct {
	Sort      string `json:"sort"`
	Value     string `json:"value"`
	Secondary string `json:"secondary,omitempty"`
	ID        string `json:"id"`
}

// SearchSessions applies FTS, filters, sorting, and stable cursor pagination.
func (s *Store) SearchSessions(ctx context.Context, query SearchQuery) (SearchResult, error) {
	if err := validateSearchQuery(query); err != nil {
		return SearchResult{}, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	effectiveSort := query.Sort
	if effectiveSort == "" {
		if strings.TrimSpace(query.Text) != "" {
			effectiveSort = "relevance"
		} else {
			effectiveSort = "last_active"
		}
	}
	cursor, err := decodeSearchCursor(query.Cursor, effectiveSort)
	if err != nil {
		return SearchResult{}, err
	}

	var matches map[string]textMatch
	if strings.TrimSpace(query.Text) != "" {
		matches, err = s.textMatches(ctx, query.Text, query.IncludeMessages)
		if err != nil {
			return SearchResult{}, err
		}
		if len(matches) == 0 {
			return SearchResult{}, nil
		}
	}

	sessions, err := s.filteredSessions(ctx, query)
	if err != nil {
		return SearchResult{}, err
	}
	hits := make([]SessionHit, 0, len(sessions))
	for _, session := range sessions {
		match, ok := matches[session.ID]
		if matches != nil && !ok {
			continue
		}
		hits = append(hits, SessionHit{Session: session, Snippet: match.snippet, Score: match.score})
	}
	sortHits(hits, effectiveSort)
	if cursor != nil {
		filtered := hits[:0]
		for _, hit := range hits {
			if hitAfterCursor(hit, effectiveSort, *cursor) {
				filtered = append(filtered, hit)
			}
		}
		hits = filtered
	}

	result := SearchResult{}
	if len(hits) <= query.Limit {
		result.Hits = hits
		return result, nil
	}
	result.Hits = hits[:query.Limit]
	next, err := encodeSearchCursor(cursorForHit(result.Hits[len(result.Hits)-1], effectiveSort))
	if err != nil {
		return SearchResult{}, err
	}
	result.NextCursor = next
	return result, nil
}

const searchDocumentEligibility = `(
    session_fts.document_type IN ('session', 'segment')
    OR (
        session_fts.document_type = 'message'
        AND (
            ? = 1
            OR sessions.analysis_provider IS NULL
            OR search_message.sequence > sessions.analyzed_through_sequence
        )
    )
)`

func (s *Store) textMatches(ctx context.Context, text string, includeMessages bool) (map[string]textMatch, error) {
	matchQuery := safeFTSQuery(text)
	if matchQuery == "" {
		return s.literalMatches(ctx, text, includeMessages)
	}
	matchQuery = "{title body} : (" + matchQuery + ")"
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_fts.session_id,
               coalesce(nullif(snippet(session_fts, 4, '', '', '...', 18), ''), session_fts.title, ''),
               bm25(session_fts, 0.0, 0.0, 0.0, 10.0, 3.0, 0.0) AS score
        FROM session_fts
        JOIN sessions ON sessions.id = session_fts.session_id
        LEFT JOIN messages AS search_message
          ON session_fts.document_type = 'message'
         AND search_message.session_id = session_fts.session_id
         AND session_fts.document_key = printf('message:%d', search_message.sequence)
        WHERE session_fts MATCH ?
          AND `+searchDocumentEligibility+`
        ORDER BY score`, matchQuery, includeMessages)
	if err != nil {
		return s.literalMatches(ctx, text, includeMessages)
	}
	defer rows.Close()
	matches := make(map[string]textMatch)
	for rows.Next() {
		var sessionID, snippet string
		var score float64
		if err := rows.Scan(&sessionID, &snippet, &score); err != nil {
			return nil, fmt.Errorf("scan FTS match: %w", err)
		}
		if _, exists := matches[sessionID]; !exists {
			matches[sessionID] = textMatch{snippet: snippet, score: score}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query FTS matches: %w", err)
	}
	return matches, nil
}

func (s *Store) literalMatches(ctx context.Context, text string, includeMessages bool) (map[string]textMatch, error) {
	pattern := "%" + escapeLike(strings.TrimSpace(text)) + "%"
	rows, err := s.db.QueryContext(ctx, `
        SELECT session_fts.session_id,
               substr(coalesce(nullif(session_fts.body, ''), session_fts.title, ''), 1, 240)
        FROM session_fts
        JOIN sessions ON sessions.id = session_fts.session_id
        LEFT JOIN messages AS search_message
          ON session_fts.document_type = 'message'
         AND search_message.session_id = session_fts.session_id
         AND session_fts.document_key = printf('message:%d', search_message.sequence)
        WHERE lower(session_fts.title || char(10) || session_fts.body)
              LIKE lower(?) ESCAPE '\'
          AND `+searchDocumentEligibility, pattern, includeMessages)
	if err != nil {
		return nil, fmt.Errorf("query literal matches: %w", err)
	}
	defer rows.Close()
	matches := make(map[string]textMatch)
	for rows.Next() {
		var sessionID, snippet string
		if err := rows.Scan(&sessionID, &snippet); err != nil {
			return nil, fmt.Errorf("scan literal match: %w", err)
		}
		if _, exists := matches[sessionID]; !exists {
			matches[sessionID] = textMatch{snippet: snippet}
		}
	}
	return matches, rows.Err()
}

func (s *Store) filteredSessions(ctx context.Context, query SearchQuery) ([]Session, error) {
	statement := sessionSelect
	var filters []string
	var args []any
	if query.Agent != "" {
		filters = append(filters, "agent = ?")
		args = append(args, query.Agent)
	}
	if query.ActiveAfter != nil {
		filters = append(filters, "last_active_at >= ?")
		args = append(args, formatTime(*query.ActiveAfter))
	}
	if query.ActiveBefore != nil {
		filters = append(filters, "last_active_at <= ?")
		args = append(args, formatTime(*query.ActiveBefore))
	}
	if query.StartedAfter != nil {
		filters = append(filters, "started_at >= ?")
		args = append(args, formatTime(*query.StartedAfter))
	}
	if query.StartedBefore != nil {
		filters = append(filters, "started_at <= ?")
		args = append(args, formatTime(*query.StartedBefore))
	}
	if query.CWD != "" {
		filters = append(filters, "instr(lower(working_directory), lower(?)) > 0")
		args = append(args, query.CWD)
	}
	switch query.TopicMode {
	case "focused":
		filters = append(filters, "topic_count IS NOT NULL AND topic_count <= 1")
	case "multiple":
		filters = append(filters, "topic_count > 1")
	}
	if query.AnalysisStatus != "" {
		filters = append(filters, "analysis_status = ?")
		args = append(args, query.AnalysisStatus)
	}
	switch query.Cmux {
	case "open":
		filters = append(filters, `EXISTS (
            SELECT 1 FROM cmux_session_state
            WHERE cmux_session_state.session_id = sessions.id
              AND cmux_session_state.open = 1
        )`)
	case "closed":
		filters = append(filters, `NOT EXISTS (
            SELECT 1 FROM cmux_session_state
            WHERE cmux_session_state.session_id = sessions.id
              AND cmux_session_state.open = 1
        )`)
	}
	if len(filters) != 0 {
		statement += " WHERE " + strings.Join(filters, " AND ")
	}
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query filtered sessions: %w", err)
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan filtered session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query filtered sessions: %w", err)
	}
	return sessions, nil
}

// SessionFacets returns global counts and date bounds for filter controls.
func (s *Store) SessionFacets(ctx context.Context) (Facets, error) {
	facets := Facets{Agents: make(map[string]int), AnalysisStates: make(map[string]int)}
	rows, err := s.db.QueryContext(ctx, `SELECT agent, count(*) FROM sessions GROUP BY agent`)
	if err != nil {
		return Facets{}, fmt.Errorf("query agent facets: %w", err)
	}
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			rows.Close()
			return Facets{}, err
		}
		facets.Agents[value] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Facets{}, fmt.Errorf("query agent facets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Facets{}, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT analysis_status, count(*) FROM sessions GROUP BY analysis_status`)
	if err != nil {
		return Facets{}, fmt.Errorf("query analysis facets: %w", err)
	}
	for rows.Next() {
		var value string
		var count int
		if err := rows.Scan(&value, &count); err != nil {
			rows.Close()
			return Facets{}, err
		}
		facets.AnalysisStates[value] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Facets{}, fmt.Errorf("query analysis facets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Facets{}, err
	}
	rows, err = s.db.QueryContext(ctx, `
        SELECT working_directory, count(*) AS count
        FROM sessions GROUP BY working_directory ORDER BY count DESC, working_directory LIMIT 20`)
	if err != nil {
		return Facets{}, fmt.Errorf("query directory facets: %w", err)
	}
	for rows.Next() {
		var facet FacetCount
		if err := rows.Scan(&facet.Value, &facet.Count); err != nil {
			rows.Close()
			return Facets{}, err
		}
		facets.Directories = append(facets.Directories, facet)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Facets{}, fmt.Errorf("query directory facets: %w", err)
	}
	if err := rows.Close(); err != nil {
		return Facets{}, err
	}
	var startedMin, activeMax *string
	if err := s.db.QueryRowContext(ctx, `SELECT min(started_at), max(last_active_at) FROM sessions`).Scan(&startedMin, &activeMax); err != nil {
		return Facets{}, fmt.Errorf("query date facets: %w", err)
	}
	if startedMin != nil {
		facets.StartedMin, err = parseTime(*startedMin)
		if err != nil {
			return Facets{}, err
		}
	}
	if activeMax != nil {
		facets.LastActiveMax, err = parseTime(*activeMax)
		if err != nil {
			return Facets{}, err
		}
	}
	return facets, nil
}

func validateSearchQuery(query SearchQuery) error {
	if len(query.Text) > 1000 {
		return errors.New("search text exceeds 1000 characters")
	}
	if len(query.CWD) > 4096 {
		return errors.New("cwd filter exceeds 4096 characters")
	}
	if query.Agent != "" && query.Agent != "codex" && query.Agent != "claude" {
		return fmt.Errorf("invalid agent %q", query.Agent)
	}
	if query.TopicMode != "" && query.TopicMode != "focused" && query.TopicMode != "multiple" {
		return fmt.Errorf("invalid topic mode %q", query.TopicMode)
	}
	validStatus := map[string]bool{"": true, "none": true, "queued": true, "running": true, "current": true, "partial": true, "failed": true}
	if !validStatus[query.AnalysisStatus] {
		return fmt.Errorf("invalid analysis status %q", query.AnalysisStatus)
	}
	if query.Cmux != "" && query.Cmux != "open" && query.Cmux != "closed" {
		return fmt.Errorf("invalid cmux state %q", query.Cmux)
	}
	if query.Sort != "" && query.Sort != "last_active" && query.Sort != "started" && query.Sort != "title" {
		return fmt.Errorf("invalid sort %q", query.Sort)
	}
	if query.Limit < 0 || query.Limit > 200 {
		return fmt.Errorf("limit %d is outside 1..200", query.Limit)
	}
	if query.ActiveAfter != nil && query.ActiveBefore != nil && query.ActiveAfter.After(*query.ActiveBefore) {
		return errors.New("active_after is after active_before")
	}
	if query.StartedAfter != nil && query.StartedBefore != nil && query.StartedAfter.After(*query.StartedBefore) {
		return errors.New("started_after is after started_before")
	}
	return nil
}

func safeFTSQuery(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	var terms []string
	for _, word := range words {
		if word == "" {
			continue
		}
		term := `"` + strings.ReplaceAll(word, `"`, `""`) + `"`
		if len([]rune(word)) >= 2 {
			term += "*"
		}
		terms = append(terms, term)
	}
	return strings.Join(terms, " AND ")
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func sortHits(hits []SessionHit, sortName string) {
	sort.SliceStable(hits, func(i, j int) bool {
		left, right := hits[i], hits[j]
		switch sortName {
		case "relevance":
			if left.Score != right.Score {
				return left.Score < right.Score
			}
			if !left.Session.LastActiveAt.Equal(right.Session.LastActiveAt) {
				return left.Session.LastActiveAt.After(right.Session.LastActiveAt)
			}
		case "started":
			if !left.Session.StartedAt.Equal(right.Session.StartedAt) {
				return left.Session.StartedAt.After(right.Session.StartedAt)
			}
		case "title":
			leftTitle, rightTitle := displayTitle(left.Session), displayTitle(right.Session)
			if leftTitle != rightTitle {
				return leftTitle < rightTitle
			}
		default:
			if !left.Session.LastActiveAt.Equal(right.Session.LastActiveAt) {
				return left.Session.LastActiveAt.After(right.Session.LastActiveAt)
			}
		}
		return left.Session.ID < right.Session.ID
	})
}

func cursorForHit(hit SessionHit, sortName string) searchCursor {
	cursor := searchCursor{Sort: sortName, ID: hit.Session.ID}
	switch sortName {
	case "relevance":
		cursor.Value = strconv.FormatFloat(hit.Score, 'g', 17, 64)
		cursor.Secondary = formatTime(hit.Session.LastActiveAt)
	case "started":
		cursor.Value = formatTime(hit.Session.StartedAt)
	case "title":
		cursor.Value = displayTitle(hit.Session)
	default:
		cursor.Value = formatTime(hit.Session.LastActiveAt)
	}
	return cursor
}

func hitAfterCursor(hit SessionHit, sortName string, cursor searchCursor) bool {
	current := cursorForHit(hit, sortName)
	switch sortName {
	case "relevance":
		left, _ := strconv.ParseFloat(current.Value, 64)
		right, _ := strconv.ParseFloat(cursor.Value, 64)
		if left != right {
			return left > right
		}
		if current.Secondary != cursor.Secondary {
			return current.Secondary < cursor.Secondary
		}
	case "started", "last_active":
		if current.Value != cursor.Value {
			return current.Value < cursor.Value
		}
	case "title":
		if current.Value != cursor.Value {
			return current.Value > cursor.Value
		}
	}
	return current.ID > cursor.ID
}

func displayTitle(session Session) string {
	if session.Title != "" {
		return strings.ToLower(session.Title)
	}
	return strings.ToLower(session.NativeSessionID)
}

func encodeSearchCursor(cursor searchCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeSearchCursor(value, sortName string) (*searchCursor, error) {
	if value == "" {
		return nil, nil
	}
	if len(value) > 2048 {
		return nil, errors.New("invalid search cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, errors.New("invalid search cursor")
	}
	var cursor searchCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.Sort != sortName || cursor.ID == "" || cursor.Value == "" {
		return nil, errors.New("invalid search cursor")
	}
	switch sortName {
	case "relevance":
		if _, err := strconv.ParseFloat(cursor.Value, 64); err != nil {
			return nil, errors.New("invalid search cursor")
		}
		if _, err := time.Parse(time.RFC3339Nano, cursor.Secondary); err != nil {
			return nil, errors.New("invalid search cursor")
		}
	case "started", "last_active":
		if _, err := time.Parse(time.RFC3339Nano, cursor.Value); err != nil {
			return nil, errors.New("invalid search cursor")
		}
	}
	return &cursor, nil
}
