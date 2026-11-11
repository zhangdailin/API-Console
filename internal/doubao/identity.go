package doubao

import (
	"crypto/rand"
	"encoding/binary"
	"math/big"

	"orchids-api/internal/store"
)

// NewIdentity makes the 19-digit identifiers emitted by the web application.
func NewIdentity() (deviceID, webID, teaUUID string, err error) {
	deviceID, err = id19()
	if err != nil {
		return "", "", "", err
	}
	webID, err = id19()
	if err != nil {
		return "", "", "", err
	}
	return deviceID, webID, webID, nil
}

func id19() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	n := new(big.Int).SetUint64(binary.BigEndian.Uint64(raw[:]))
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	span := new(big.Int).Mul(big.NewInt(9), base)
	n.Mod(n, span).Add(n, base)
	return n.String(), nil
}

func identityFor(acc *store.Account) (string, string, string) {
	if acc != nil && acc.DoubaoDeviceID != "" && acc.DoubaoWebID != "" {
		return acc.DoubaoDeviceID, acc.DoubaoWebID, acc.DoubaoTeaUUID
	}
	deviceID, webID, teaUUID, _ := NewIdentity()
	return deviceID, webID, teaUUID
}
