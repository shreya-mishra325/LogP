package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"browsercompute.local/runtime/internal/workload"
)

const Version = 1

type Frame struct {
	V         int             `json:"v"`
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Out struct {
	V         int    `json:"v"`
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	Payload   any    `json:"payload"`
}

type Hello struct {
	Token   string   `json:"worker_token"`
	Slots   int      `json:"slots"`
	Kernels []string `json:"supported_kernels"`
}

type Welcome struct {
	Session          string `json:"session_id"`
	Heartbeat        int64  `json:"heartbeat_ms"`
	HeartbeatTimeout int64  `json:"heartbeat_timeout_ms"`
	TaskTimeout      int64  `json:"task_timeout_ms"`
}

type Ready struct {
	Slots *int `json:"available_slots"`
}

type Lease struct {
	JobID   string `json:"job_id"`
	TaskID  string `json:"task_id"`
	LeaseID string `json:"lease_id"`
}

func (l Lease) Valid() bool {
	return len(l.JobID) > 0 && len(l.JobID) <= 64 && len(l.TaskID) > 0 && len(l.TaskID) <= 64 && len(l.LeaseID) > 0 && len(l.LeaseID) <= 64
}

type Assignment struct {
	Lease
	Attempt       int             `json:"attempt"`
	Kernel        string          `json:"kernel"`
	KernelVersion int             `json:"kernel_version"`
	Params        workload.Params `json:"params"`
	Deadline      int64           `json:"deadline_ms"`
}

type Result struct {
	Lease
	Data *string `json:"data_b64"`
}

type TaskError struct {
	Lease
	Code    string  `json:"code"`
	Message *string `json:"message"`
}

type Ack struct {
	LeaseID     string `json:"lease_id"`
	Disposition string `json:"disposition"`
}

type Cancel struct {
	LeaseID string `json:"lease_id"`
	Reason  string `json:"reason"`
}

func Decode(data []byte, dst any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' || !utf8.Valid(data) {
		return errors.New("expected JSON object")
	}
	// All protocol objects are flat. Reject duplicate and differently cased keys
	// before encoding/json can silently replace or fold them.
	keys := make(map[string]struct{})
	fields := json.NewDecoder(bytes.NewReader(data))
	_, _ = fields.Token()
	for fields.More() {
		if len(keys) == 16 {
			return errors.New("too many JSON fields")
		}
		token, err := fields.Token()
		key, ok := token.(string)
		if err != nil || !ok || key != strings.ToLower(key) {
			return errors.New("invalid JSON field")
		}
		if _, exists := keys[key]; exists {
			return errors.New("duplicate JSON field")
		}
		keys[key] = struct{}{}
		var value json.RawMessage
		if err := fields.Decode(&value); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func Empty(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	return Decode(data, &struct{}{}) == nil
}
