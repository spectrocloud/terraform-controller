package util

import (
	"fmt"
	"github.com/mitchellh/hashstructure"
)

func GenerateHash(input interface{}) string {
	var uid = ""
	if hash, err := hashstructure.Hash(input, nil); err == nil {
		uid = fmt.Sprintf("%v", hash)
	}
	return uid
}
