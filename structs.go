package themis

import "time"

// Key implements a key document.
type Key struct {
	ServerKey string    `bson:"_id"`
	ClientKey string    `bson:"clientKey"`
	LastUsed  time.Time `bson:"lastUsed"`
}

// Recipient returns the recipient from a key pair given the sender.
func (k Key) Recipient(sender string) string {
	if sender == k.ServerKey {
		return k.ClientKey
	}
	return k.ServerKey
}

// File stores the content of a file.
type File struct {
	Checksum string `bson:"_id"`
	RecvKey  string `bson:"recvKey"`
	Filename string `bson:"filename"`
	Content  []byte `bson:"content"`
}

// Update represents an event
type Update struct {
	Receiver string
	Type     string
	Message  string
}
