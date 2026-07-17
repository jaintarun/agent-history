package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jaintarun/agent-history/internal/analyze"
)

func TestEvaluationUsesFakeAnalyzerAndReportsMetrics(t *testing.T) {
	report, err := Run(context.Background(), metricFakeAnalyzer{}, "fake-model")
	if err != nil {
		t.Fatal(err)
	}
	if report.Model != "fake-model" || len(report.Cases) != 2 {
		t.Fatalf("report = %#v", report)
	}
	for _, result := range report.Cases {
		if result.InitialCalls == 0 || result.ApproximateInputTokens == 0 || !result.HiddenTermsExcluded || !result.AppendEfficient {
			t.Fatalf("case metrics = %#v", result)
		}
	}
}

type metricFakeAnalyzer struct{}

func (metricFakeAnalyzer) Generate(_ context.Context, _ string, request analyze.StructuredRequest) (json.RawMessage, error) {
	switch request.Kind {
	case analyze.RequestLeaf:
		return json.RawMessage(`{"title":"Migration leaf","goal":"Complete migration","summary":"Authentication retry vault search migration documentation verification.","detail":"Completed grounded work.","outcome":"complete","entities":[],"files":[],"errors":[],"evidence":[]}`), nil
	case analyze.RequestBoundary:
		return json.RawMessage(`{"same_topic":true,"confidence":0.9,"new_title":""}`), nil
	case analyze.RequestTopic, analyze.RequestRollup:
		return json.RawMessage(`{"title":"Migration topic","summary":"Authentication retry vault search migration documentation verification.","detail":"Grounded topic detail.","evidence":[]}`), nil
	case analyze.RequestSession:
		return json.RawMessage(`{"title":"Authentication vault migration","summary":"Authentication retry, vault search, migration, documentation, and verification."}`), nil
	default:
		return nil, errors.New("unexpected evaluation request")
	}
}
