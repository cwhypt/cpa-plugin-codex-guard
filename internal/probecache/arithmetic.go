package probecache

import (
	"encoding/json"
	"regexp"
	"strconv"
)

const arithmeticInstructions = "You are a provider health-check endpoint. Answer the arithmetic challenge exactly and briefly."

var arithmeticProbe = regexp.MustCompile(`\ACalculate and respond with ONLY the number, nothing else\.\n\nQ: 3 \+ 5 = \?\nA: 8\n\nQ: 12 - 7 = \?\nA: 5\n\nQ: ([0-9]{1,2}) ([+-]) ([0-9]{1,2}) = \?\nA:\z`)

// ArithmeticProbe solves only the observed provider health-check template.
func ArithmeticProbe(body []byte) (string, bool) {
	var root map[string]json.RawMessage
	if len(body) > 20480 || json.Unmarshal(body, &root) != nil {
		return "", false
	}
	for key := range root {
		switch key {
		case "model", "input", "instructions", "stream", "max_output_tokens":
		default:
			return "", false
		}
	}
	var model, instructions, input string
	var stream bool
	var maxTokens int
	if json.Unmarshal(root["model"], &model) != nil || model != "gpt-5.6-sol" ||
		json.Unmarshal(root["instructions"], &instructions) != nil || instructions != arithmeticInstructions ||
		json.Unmarshal(root["input"], &input) != nil || json.Unmarshal(root["stream"], &stream) != nil ||
		(string(root["stream"]) != "true" && string(root["stream"]) != "false") ||
		json.Unmarshal(root["max_output_tokens"], &maxTokens) != nil || maxTokens != 50 {
		return "", false
	}
	match := arithmeticProbe.FindStringSubmatch(input)
	if match == nil {
		return "", false
	}
	left, _ := strconv.Atoi(match[1])
	right, _ := strconv.Atoi(match[3])
	if match[2] == "-" {
		return strconv.Itoa(left - right), true
	}
	return strconv.Itoa(left + right), true
}
