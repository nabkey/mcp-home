package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, "info", "json").Info("hello", "k", "v")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not JSON: %v: %q", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("unexpected record: %v", rec)
	}
}

func TestNewLoggerText(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf, "info", "text").Info("hello", "k", "v")

	if out := buf.String(); !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("unexpected text output: %q", out)
	}
}

func TestNewLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := newLogger(&buf, "warn", "json")
	logger.Info("dropped")
	logger.Warn("kept")

	if out := buf.String(); strings.Contains(out, "dropped") || !strings.Contains(out, "kept") {
		t.Errorf("level not applied: %q", out)
	}
}
