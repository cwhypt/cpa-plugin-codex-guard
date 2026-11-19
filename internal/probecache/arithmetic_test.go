package probecache

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestArithmeticProbe(t *testing.T) {
	base := map[string]any{
		"model": "gpt-5.6-sol", "stream": true, "max_output_tokens": 50,
		"instructions": arithmeticInstructions,
	}
	for _, tc := range []struct{ expression, want string }{
		{"24 - 9", "15"}, {"48 + 24", "72"}, {"1 - 9", "-8"},
		{"1 * 9", ""}, {"123 + 1", ""},
	} {
		base["input"] = "Calculate and respond with ONLY the number, nothing else.\n\nQ: 3 + 5 = ?\nA: 8\n\nQ: 12 - 7 = ?\nA: 5\n\nQ: " + tc.expression + " = ?\nA:"
		body, _ := json.Marshal(base)
		got, ok := ArithmeticProbe(body)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got %q, %v", tc.expression, got, ok)
		}
	}
	base["input"] = "Calculate and respond with ONLY the number, nothing else.\n\nQ: 3 + 5 = ?\nA: 8\n\nQ: 12 - 7 = ?\nA: 5\n\nQ: 21 + 30 = ?\nA:"
	base["tools"] = []any{}
	body, _ := json.Marshal(base)
	if _, ok := ArithmeticProbe(body); ok {
		t.Fatal("requests with additional fields must pass through")
	}
	delete(base, "tools")
	base["instructions"] = strings.ToUpper(arithmeticInstructions)
	body, _ = json.Marshal(base)
	if _, ok := ArithmeticProbe(body); ok {
		t.Fatal("different instructions must pass through")
	}
}
