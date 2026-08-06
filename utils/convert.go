package utils

import "strings"

func GetStringSlice(concatenated string, args ...string) []string {
	delimiter := ";"
	if len(args) > 0 {
		delimiter = args[0]
	}

	split := strings.Split(concatenated, delimiter)
	slice := make([]string, 0, len(split))
	for _, entry := range split {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			slice = append(slice, trimmed)
		}
	}

	return slice
}
