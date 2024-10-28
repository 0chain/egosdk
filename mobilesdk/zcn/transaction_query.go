//go:build mobile
// +build mobile

package zcn

import (
	"github.com/0chain/egosdk/zcncore"
)

// GetUserLockedTotal get total token user locked
// # Inputs
//   - clientID wallet id
func GetUserLockedTotal(clientID string) (int64, error) {
	return zcncore.GetUserLockedTotal(clientID)
}
