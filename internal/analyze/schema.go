package analyze

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

var schemas = map[RequestKind]json.RawMessage{
	RequestLeaf: json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "properties":{
    "title":{"type":"string","maxLength":120},
    "goal":{"type":"string","maxLength":400},
    "summary":{"type":"string","maxLength":800},
    "detail":{"type":"string","maxLength":5000},
    "outcome":{"type":"string","maxLength":400},
    "entities":{"type":"array","maxItems":30,"items":{"type":"string","maxLength":160}},
    "files":{"type":"array","maxItems":50,"items":{"type":"string","maxLength":500}},
    "errors":{"type":"array","maxItems":20,"items":{"type":"string","maxLength":500}},
    "evidence":{"type":"array","maxItems":50,"items":{"type":"integer","minimum":0}}
  },
  "required":["title","goal","summary","detail","outcome","entities","files","errors","evidence"]
}`),
	RequestBoundary: json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "properties":{
    "same_topic":{"type":"boolean"},
    "confidence":{"type":"number","minimum":0,"maximum":1},
    "new_title":{"type":"string","maxLength":120}
  },
  "required":["same_topic","confidence","new_title"]
}`),
	RequestRollup: rollupSchema(),
	RequestTopic:  rollupSchema(),
	RequestSession: json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "properties":{
    "title":{"type":"string","maxLength":160},
    "summary":{"type":"string","maxLength":1000}
  },
  "required":["title","summary"]
}`),
}

func rollupSchema() json.RawMessage {
	return json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "properties":{
    "title":{"type":"string","maxLength":120},
    "summary":{"type":"string","maxLength":1000},
    "detail":{"type":"string","maxLength":6000},
    "evidence":{"type":"array","maxItems":100,"items":{"type":"integer","minimum":0}}
  },
  "required":["title","summary","detail","evidence"]
}`)
}

func decodeLeaf(raw json.RawMessage, start, end int) (leafSummary, error) {
	var result leafSummary
	if err := decodeStrict(raw, &result); err != nil {
		return leafSummary{}, err
	}
	if err := validateText(result.Title, "leaf title", 120); err != nil {
		return leafSummary{}, err
	}
	if err := validateText(result.Summary, "leaf summary", 800); err != nil {
		return leafSummary{}, err
	}
	if err := validateText(result.Detail, "leaf detail", 5000); err != nil {
		return leafSummary{}, err
	}
	if err := validateEvidence(result.Evidence, start, end); err != nil {
		return leafSummary{}, err
	}
	return result, nil
}

func decodeRollup(raw json.RawMessage, start, end int) (rollupSummary, error) {
	var result rollupSummary
	if err := decodeStrict(raw, &result); err != nil {
		return rollupSummary{}, err
	}
	if err := validateText(result.Title, "rollup title", 120); err != nil {
		return rollupSummary{}, err
	}
	if err := validateText(result.Summary, "rollup summary", 1000); err != nil {
		return rollupSummary{}, err
	}
	if err := validateText(result.Detail, "rollup detail", 6000); err != nil {
		return rollupSummary{}, err
	}
	if err := validateEvidence(result.Evidence, start, end); err != nil {
		return rollupSummary{}, err
	}
	return result, nil
}

func decodeSession(raw json.RawMessage) (sessionSummary, error) {
	var result sessionSummary
	if err := decodeStrict(raw, &result); err != nil {
		return sessionSummary{}, err
	}
	if err := validateText(result.Title, "session title", 160); err != nil {
		return sessionSummary{}, err
	}
	if err := validateText(result.Summary, "session summary", 1000); err != nil {
		return sessionSummary{}, err
	}
	return result, nil
}

func decodeBoundary(raw json.RawMessage) (modelBoundary, error) {
	var result modelBoundary
	if err := decodeStrict(raw, &result); err != nil {
		return modelBoundary{}, err
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return modelBoundary{}, errors.New("boundary confidence is outside 0..1")
	}
	if len(result.NewTitle) > 120 {
		return modelBoundary{}, errors.New("boundary title exceeds 120 characters")
	}
	return result, nil
}

func decodeStrict(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode structured analysis: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("structured analysis contains trailing JSON")
	}
	return nil
}

func validateText(value, field string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if utf8.RuneCountInString(value) > limit {
		return fmt.Errorf("%s exceeds %d characters", field, limit)
	}
	return nil
}

func validateEvidence(evidence []int, start, end int) error {
	for _, sequence := range evidence {
		if sequence < start || sequence > end {
			return fmt.Errorf("evidence sequence %d is outside %d..%d", sequence, start, end)
		}
	}
	return nil
}
