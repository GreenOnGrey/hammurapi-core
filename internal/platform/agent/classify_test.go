package agent

import "testing"

// ERR-01…06, ERR-10 (unit part): Pi's errorMessage formats classified by status and text.
func TestClassify(t *testing.T) {
	cases := []struct {
		in     string
		class  ErrorClass
		status int
		msg    string
	}{
		{`402: {"message":"Insufficient Balance","type":"api_error","code":"402"}`, ErrInsufficientBalance, 402, "Insufficient Balance"},
		{`401: {"message":"Authentication Fails, Your api key: ****abcd is invalid"}`, ErrAuth, 401, "Authentication Fails, Your api key: ****abcd is invalid"},
		{`403: forbidden`, ErrAuth, 403, "forbidden"},
		{`429: {"message":"Rate limit reached"}`, ErrRateLimit, 429, "Rate limit reached"},
		{`503: {"message":"Service Unavailable"}`, ErrUnavailable, 503, "Service Unavailable"},
		{`500: oops`, ErrUnavailable, 500, "oops"},
		{`400: {"message":"This model's maximum context length is 131072 tokens. However, you requested 200000 tokens"}`, ErrContextOverflow, 400, ""},
		{`400: {"error":{"message":"Invalid request"}}`, ErrBadRequest, 400, "Invalid request"},
		{`422: unprocessable`, ErrBadRequest, 422, "unprocessable"},
		{`Request timed out after 300000 ms`, ErrUnavailable, 0, ""},
		{`fetch failed`, ErrUnavailable, 0, ""},
		{`Your account has insufficient balance`, ErrInsufficientBalance, 0, ""},
		{`something odd`, ErrBadRequest, 0, "something odd"},
	}
	for _, c := range cases {
		got := Classify(c.in)
		if got.Class != c.class || got.HTTPStatus != c.status {
			t.Errorf("%q → %s %d, want %s %d", c.in, got.Class, got.HTTPStatus, c.class, c.status)
		}
		if c.msg != "" && got.Message != c.msg {
			t.Errorf("%q message %q, want %q", c.in, got.Message, c.msg)
		}
	}
}

func TestClassBehaviour(t *testing.T) {
	if ErrInsufficientBalance.Retryable() || ErrAuth.Retryable() || ErrBadRequest.Retryable() {
		t.Fatal("connection and request errors are not retryable by the user")
	}
	if !ErrRateLimit.Retryable() || !ErrUnavailable.Retryable() {
		t.Fatal("transient errors are retryable")
	}
	if !ErrInsufficientBalance.ConnectionProblem() || !ErrAuth.ConnectionProblem() || ErrRateLimit.ConnectionProblem() {
		t.Fatal("connection problems")
	}
}

func TestScenarios(t *testing.T) {
	if !ScenarioCodegen.HasCode() || ScenarioChat.HasCode() || ScenarioIssueAnalysis.HasCode() {
		t.Fatal("code scenarios")
	}
	if _, err := ParseScenario("chat"); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseScenario("hack"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}
