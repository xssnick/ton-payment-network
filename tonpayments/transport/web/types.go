package web

import "crypto/ed25519"

type Event struct {
	Key     ed25519.PublicKey `json:"key"`
	QueryID string            `json:"query_id,omitempty"`
	Data    []byte            `json:"data"`
}

type SubscribeAuth struct {
	PeerKey   ed25519.PublicKey `json:"key"`
	Timestamp int64             `json:"timestamp"`
	Signature []byte            `json:"signature"`
}

type SubscribeAuthResult struct {
	Token string `json:"token"`
}

type QueryResponseAccepted struct {
	Success bool `json:"success"`
}
