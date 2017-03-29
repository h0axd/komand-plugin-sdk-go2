package message

import "encoding/json"

// V1 is the general purpose komand plugin message envelope, which contains the body and other metadata
type V1 struct {
	Version string      `json:"version"`
	Type    string      `json:"type"`
	Body    interface{} `json:"body"`
}

// BodyV1 is the V1 message body
type BodyV1 struct {
	Meta json.RawMessage `json:"meta"`
	// Of Action and Trigger, only one will be set at a time.
	Action     string                 `json:"action"`
	Trigger    string                 `json:"trigger"`
	Connection json.RawMessage        `json:"connection"` // connection.Data is defined per plugin, so it will be serialized individually
	Dispatcher map[string]interface{} `json:"dispatcher"` // Dispatcher is one of a few options, but we need to pull metadata from it to know what, so we use m[s]i{}
	Input      json.RawMessage        `json:"input"`      // Inputs are defined per action, so they will be serialized individually
}
