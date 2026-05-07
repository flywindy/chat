package natsrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrUnavailable_HasCodeAndMessage(t *testing.T) {
	err := ErrUnavailable("service busy")
	assert.Equal(t, "unavailable", err.Code)
	assert.Equal(t, "service busy", err.Message)
}

func TestCodeUnavailable_UsedByErrUnavailable(t *testing.T) {
	err := ErrUnavailable("any message")
	assert.Equal(t, CodeUnavailable, err.Code)
}
