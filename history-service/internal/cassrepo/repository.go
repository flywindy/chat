package cassrepo

import (
	"fmt"

	"github.com/gocql/gocql"
)

// casMaxRetries bounds the CAS loop; 16 retries cover realistic burst concurrency.
const casMaxRetries = 16

// casDecrement atomically decrements a nullable INT toward zero (clamping at zero); mirrors message-worker/store_cassandra.go casIncrement.
func casDecrement(maxRetries int, initial *int, update func(newVal int, expected *int) (applied bool, current *int, err error)) error {
	tcount := initial
	for range maxRetries {
		newVal := 0
		if tcount != nil && *tcount > 0 {
			newVal = *tcount - 1
		}
		applied, current, err := update(newVal, tcount)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		tcount = current
	}
	return fmt.Errorf("cas decrement exceeded %d retries", maxRetries)
}

type Repository struct {
	session *gocql.Session
}

func NewRepository(session *gocql.Session) *Repository {
	return &Repository{session: session}
}
