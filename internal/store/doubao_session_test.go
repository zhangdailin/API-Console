package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDoubaoSessionPatchOnlyChangesCookieAndVersion(t *testing.T) {
	s, _ := newDoubaoIdentityTestStore(t)
	acc := doubaoIdentityTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDoubaoSession(t.Context(), acc.ID, DoubaoSessionPatch{Expected: DoubaoSessionVersion(before), CookieChanged: true, Cookie: `{"sessionid":"synthetic-rotated"}`}); err != nil {
		t.Fatal(err)
	}
	after, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := before.Clone()
	expected.ClientCookie, expected.UpdatedAt = after.ClientCookie, after.UpdatedAt
	if !reflect.DeepEqual(expected, after) {
		t.Fatal("cookie commit changed unrelated state")
	}
	if err := s.UpdateDoubaoSession(t.Context(), acc.ID, DoubaoSessionPatch{Expected: DoubaoSessionVersion(before), Rejected: true, ObservedAt: time.Now().AddDate(10, 0, 0)}); err == nil {
		t.Fatal("obsolete cookie generation accepted")
	}
}

func TestDoubaoObservationIsPrivateAndCloneIsolated(t *testing.T) {
	acc := doubaoIdentityTestAccount()
	acc.DoubaoSessionUpdate = &DoubaoSessionObservation{Expected: DoubaoSessionVersion(acc), Healthy: true, ObservedAt: time.Now()}
	clone := acc.Clone()
	clone.DoubaoSessionUpdate.Healthy = false
	if !acc.DoubaoSessionUpdate.Healthy {
		t.Fatal("clone shares observation")
	}
	raw, err := json.Marshal(acc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Account
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.DoubaoSessionUpdate != nil {
		t.Fatal("write intent leaked through JSON")
	}
	if err := json.Unmarshal([]byte(`{"DoubaoSessionUpdate":{"Healthy":true},"doubao_session_update":{"Healthy":true}}`), &decoded); err != nil || decoded.DoubaoSessionUpdate != nil {
		t.Fatal("JSON injected a private write intent")
	}
}

func TestDoubaoSessionVersionAndInvalidPatches(t *testing.T) {
	acc := doubaoIdentityTestAccount()
	acc.ClientCookie = `{"b":"two","a":"one"}`
	clone := acc.Clone()
	clone.ClientCookie, clone.Name, clone.Enabled = `{"a":"one", "b":"two"}`, "new admin name", true
	if DoubaoSessionVersion(acc) != DoubaoSessionVersion(clone) {
		t.Fatal("canonical cookie version changed for an unrelated edit")
	}
	clone.DoubaoDeviceID = "new-device"
	if DoubaoSessionVersion(acc) == DoubaoSessionVersion(clone) {
		t.Fatal("identity rotation retained session version")
	}
	var absent *Store
	if absent.UpdateDoubaoSession(t.Context(), 1, DoubaoSessionPatch{}) == nil {
		t.Fatal("missing store accepted update")
	}
	s, _ := newDoubaoIdentityTestStore(t)
	if s.UpdateDoubaoSession(t.Context(), 1, DoubaoSessionPatch{}) == nil {
		t.Fatal("missing session evidence accepted")
	}
}
