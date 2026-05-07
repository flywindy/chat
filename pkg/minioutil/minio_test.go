package minioutil

import (
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
)

func TestBucket_RawAndName(t *testing.T) {
	client := &minio.Client{}
	b := &Bucket[struct{}]{client: client, name: "my-bucket"}
	assert.Same(t, client, b.Raw())
	assert.Equal(t, "my-bucket", b.Name())
}
