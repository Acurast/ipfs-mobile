package utils

import "fmt"

func Timeout() error {
	return fmt.Errorf("timeout")
}
