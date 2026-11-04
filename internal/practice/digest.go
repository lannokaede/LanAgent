package practice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }
