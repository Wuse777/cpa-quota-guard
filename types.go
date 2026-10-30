package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const pluginID = "quota-guard"
const version = "0.2.2"

type hostCall func(string, any, any) error

type runtimeConfig struct {
	StateDir          string `yaml:"state_dir"`
	ProxyURL          string `yaml:"proxy_url"`
	ManagementURL     string `yaml:"management_url"`
	ManagementKeyFile string `yaml:"management_key_file"`
	// Used only by the isolated integration harness; restricted to loopback HTTP.
	TestProbeBase string `yaml:"test_probe_base"`
}

type policy struct {
	Revision         uint64      `json:"revision"`
	Mode             string      `json:"mode"`
	PollSeconds      int         `json:"poll_seconds"`
	MaxAgeSeconds    int         `json:"max_age_seconds"`
	TimeoutSeconds   int         `json:"timeout_seconds"`
	SafetyPercent    float64     `json:"safety_percent"`
	ResumeGapPercent float64     `json:"resume_gap_percent"`
	UnknownAction    string      `json:"unknown_action"`
	Strategy         string      `json:"strategy"`
	Rules            []rule      `json:"rules"`
	AutoReset        resetPolicy `json:"auto_reset"`
}

type resetPolicy struct {
	Enabled     bool            `json:"enabled"`
	BeforeHours float64         `json:"before_hours"`
	Accounts    map[string]bool `json:"accounts"`
}

type rule struct {
	Account             string  `json:"account"`
	Enabled             bool    `json:"enabled"`
	MinRemainingPercent float64 `json:"min_remaining_percent"`
	// Empty selects all normal quota buckets. Explicit keys must all be present.
	Windows map[string]float64 `json:"windows,omitempty"`
}

type account struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Email     string `json:"email,omitempty"`
	Disabled  bool   `json:"disabled"`
}

type bucket struct {
	ID               string    `json:"id"`
	Label            string    `json:"label"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
	// Auxiliary quotas (code review / named model limits) require explicit selection.
	Auxiliary bool `json:"auxiliary,omitempty"`
}

type snapshot struct {
	Account   account   `json:"account"`
	Buckets   []bucket  `json:"buckets"`
	CheckedAt time.Time `json:"checked_at"`
	AttemptAt time.Time `json:"attempt_at"`
	Error     string    `json:"error,omitempty"`
}

type decision struct {
	Protected bool   `json:"protected"`
	Reason    string `json:"reason"`
}

type accountStatus struct {
	snapshot
	decision
	WouldBlock bool                `json:"would_block"`
	Reset      *resetAccountStatus `json:"reset,omitempty"`
}

type candidate struct {
	ID         string
	Provider   string
	Priority   int
	Attributes map[string]string
	Metadata   map[string]any
}

type pickRequest struct {
	Provider  string
	Providers []string
	Model     string
	Stream    bool
	Options   struct {
		Headers  http.Header
		Metadata map[string]any
	}
	Candidates []candidate
}

type pickResponse struct {
	AuthID          string
	DelegateBuiltin string
	Handled         bool
	Reject          bool
	RejectReason    string
	RejectCode      string
}

type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id"`
}

type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}
