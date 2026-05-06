package service

import (
	"encoding/json"

	"github.com/hmchangw/chat/pkg/natsrouter"
)

// EncryptEditMsgForTest is exported only for tests in another package.
// The `_test.go` suffix excludes this file from production builds.
func (s *HistoryService) EncryptEditMsgForTest(c *natsrouter.Context, roomID, plaintext string) (string, json.RawMessage, error) {
	return s.encryptEditMsg(c, roomID, plaintext)
}
