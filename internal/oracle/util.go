package oracle

import "fmt"

func sprintf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
