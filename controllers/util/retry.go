package util

import "time"

func Retry(op func() error, delay time.Duration, max int) error {
	for i := 1; i <= max || max <= 0; i++ {
		if err := op(); err == nil {
			return err
		}
		time.Sleep(delay)
	}
	return op()
}
