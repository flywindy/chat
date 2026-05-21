//go:build integration

package roomcrypto

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hmchangw/chat/pkg/testutil/testimages"
)

// decryptPayload is the JSON structure passed to the TypeScript decrypt script.
type decryptPayload struct {
	PrivateKey string            `json:"privateKey"` // base64 — 32 bytes of high-entropy IKM
	Message    *EncryptedMessage `json:"message"`
}

// setupNodeContainer starts a node:20-alpine container, copies decrypt.ts into it,
// and installs tsx. The container is terminated via t.Cleanup.
func setupNodeContainer(t *testing.T) testcontainers.Container {
	t.Helper()
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      testimages.Node,
			Cmd:        []string{"sh", "-c", "sleep 600"},
			WaitingFor: wait.ForExec([]string{"node", "--version"}).WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "start node:20-alpine container")
	t.Cleanup(func() {
		_ = container.Terminate(ctx) // best-effort; ignore cleanup errors
	})

	// Copy the TypeScript decrypt script into the container.
	scriptPath := filepath.Join("testdata", "decrypt.ts")
	err = container.CopyFileToContainer(ctx, scriptPath, "/decrypt.ts", 0o644)
	require.NoError(t, err, "copy testdata/decrypt.ts into container")

	// Install tsx (TypeScript runner). Combined stdout+stderr is captured for diagnostics.
	exitCode, reader, err := container.Exec(ctx, []string{"sh", "-c", "npm install -g tsx --quiet 2>&1"})
	require.NoError(t, err, "exec npm install tsx")
	out := readCombined(reader)
	require.Equal(t, 0, exitCode, "npm install tsx failed:\n%s", out)

	return container
}

// readCombined reads the Docker multiplexed output stream and concatenates stdout and stderr.
// Use for diagnostic output where all text is needed for failure messages.
func readCombined(r io.Reader) string {
	if r == nil {
		return ""
	}
	var stdout, stderr bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdout, &stderr, r)
	return stdout.String() + stderr.String()
}

// splitOutput reads a Docker multiplexed stream and returns stdout and combined output separately.
// stdout is used for program output assertions; combined is used for failure diagnostics.
func splitOutput(r io.Reader) (stdout, combined string) {
	if r == nil {
		return "", ""
	}
	var outBuf, errBuf bytes.Buffer
	_, _ = stdcopy.StdCopy(&outBuf, &errBuf, r)
	return outBuf.String(), outBuf.String() + errBuf.String()
}

func TestEncode_TypeScriptDecrypt(t *testing.T) {
	ctx := context.Background()
	container := setupNodeContainer(t)

	cases := []struct {
		name    string
		content string
	}{
		{name: "non-empty message", content: "hello from Go"},
		{name: "empty string", content: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Generate a fresh 32-byte room private key (high-entropy IKM) for this subtest.
			roomPriv := make([]byte, 32)
			_, err := io.ReadFull(rand.Reader, roomPriv)
			require.NoError(t, err)

			// Encrypt with Go (room private key used directly as AES-256-GCM key).
			enc := NewEncoder()
			msg, err := enc.Encode("room-integration", tc.content, roomPriv, 1)
			require.NoError(t, err)

			// Build the JSON payload the TypeScript script expects.
			payload := decryptPayload{
				PrivateKey: base64.StdEncoding.EncodeToString(roomPriv),
				Message:    msg,
			}
			payloadJSON, err := json.Marshal(payload)
			require.NoError(t, err)

			// Write payload to a per-subtest temp file and copy into the container.
			payloadFile := filepath.Join(t.TempDir(), "payload.json")
			err = os.WriteFile(payloadFile, payloadJSON, 0o600)
			require.NoError(t, err)
			err = container.CopyFileToContainer(ctx, payloadFile, "/payload.json", 0o644)
			require.NoError(t, err, "copy payload.json into container")

			// Run the TypeScript decrypt script, piping the payload via stdin.
			exitCode, reader, err := container.Exec(ctx, []string{"sh", "-c", "cat /payload.json | tsx /decrypt.ts"})
			require.NoError(t, err, "exec tsx decrypt")
			stdout, combined := splitOutput(reader)
			require.Equal(t, 0, exitCode, "decrypt script exited non-zero:\n%s", combined)

			// stdout should equal the original plaintext; trim trailing newline only.
			assert.Equal(t, tc.content, strings.TrimRight(stdout, "\n"))
		})
	}
}
