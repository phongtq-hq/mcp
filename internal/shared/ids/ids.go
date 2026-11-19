// Package ids sinh định danh cho tài nguyên của platform.
package ids

import "github.com/google/uuid"

// New trả về một UUID phiên bản 7 (sắp xếp được theo thời gian).
func New() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}
