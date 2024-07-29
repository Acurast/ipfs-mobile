package utils

import "strings"

func GetStringSlice(concatenated string, args ...string) []string {
	delimiter := ";"
	if len(args) > 0 {
		delimiter = args[0]
	}
	
	return strings.Split(concatenated, delimiter)
}