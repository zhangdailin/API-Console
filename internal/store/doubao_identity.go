package store

import (
	"context"
	"fmt"
	"strings"
)

// DoubaoIdentityPatch replaces only the browser identity, conditional on the
// exact credential and identity that the caller observed.
type DoubaoIdentityPatch struct {
	ExpectedCookie   string
	ExpectedDeviceID string
	ExpectedWebID    string
	ExpectedTeaUUID  string
	DeviceID         string
	WebID            string
	TeaUUID          string
}

// RotateDoubaoIdentity atomically replaces the three Doubao browser identifiers
// without changing credentials, verification, routing status, quota, or usage.
func (s *Store) RotateDoubaoIdentity(ctx context.Context, id int64, patch DoubaoIdentityPatch) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("doubao identity store not configured")
	}
	if err := patch.validate(); err != nil {
		return err
	}
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "doubao") {
			return fmt.Errorf("doubao identity rotation requires a Doubao account")
		}
		if acc.ClientCookie != patch.ExpectedCookie ||
			acc.DoubaoDeviceID != patch.ExpectedDeviceID ||
			acc.DoubaoWebID != patch.ExpectedWebID ||
			acc.DoubaoTeaUUID != patch.ExpectedTeaUUID {
			return fmt.Errorf("doubao credential or identity changed concurrently")
		}
		acc.DoubaoDeviceID = patch.DeviceID
		acc.DoubaoWebID = patch.WebID
		acc.DoubaoTeaUUID = patch.TeaUUID
		return nil
	}, true)
}

func (p DoubaoIdentityPatch) validate() error {
	for _, id := range []string{p.DeviceID, p.WebID, p.TeaUUID} {
		if len(id) != 19 || id[0] < '1' || id[0] > '9' {
			return fmt.Errorf("doubao identity must contain exactly 19 ASCII digits with a nonzero first digit")
		}
		for i := 1; i < len(id); i++ {
			if id[i] < '0' || id[i] > '9' {
				return fmt.Errorf("doubao identity must contain exactly 19 ASCII digits with a nonzero first digit")
			}
		}
	}
	if p.WebID != p.TeaUUID {
		return fmt.Errorf("doubao web identity and tea UUID must match")
	}
	if p.DeviceID == p.ExpectedDeviceID || p.WebID == p.ExpectedWebID {
		return fmt.Errorf("doubao identity rotation must replace both device and web identifiers")
	}
	return nil
}
